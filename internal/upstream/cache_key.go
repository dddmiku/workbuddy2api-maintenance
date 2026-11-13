// ═══ 更新日志 ═══
// 2026-11-14：区分服务端缓存能力、官方抓包形态及网关生成键的实际隔离/空会话语义。
// 2026-09-16：缓存键注入保留请求中的数字原值，并保持完整 JSON 文档校验。
// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 历史实验（buddy-adapter.ts:706-714）观察到上游支持 prompt_cache_key：
// 在所测模型、账号和同一段 8k token 前缀条件下：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，
// 让同一客户端对同一账号的连续请求复用上游前缀缓存。
// 所查 6 份官方抓包未见该字段；服务端支持它，不等于官方客户端通常发送它。
// 约 17× 的历史费用差异不是当前所有请求的保证；不据样本缺字段删除既有缓存功能。
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"workbuddy2api/internal/jsonutil"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自 X-Conversation-ID 头解析）
//
// 安全约束——按账号隔离：
//   - 生成键格式 `wb2a-<uid8>-<convHex>`
//   - uid8 仅为可读标识，不同账号可能同前缀；convHex 的哈希输入包含完整 UID 与会话源
//   - 自动生成键按账号维度分桶；显式键仍原样保留。缓存键不是授权边界，不能仅据键相同断言泄露
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的会话标识（body 里没有 conversation_id 时用它做哈希源）。
// 两源都空时，同一 UID 得到同一键，空会话源共享该账号的分桶，不会自动生成新会话键。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	// 兼容调用方可显式带 prompt_cache_key；这不代表所查官方抓包通常携带。
	// 键名字节仅作前置检查，仍需完整解码确认顶层值；命中显式键后原样返回，
	// 否则复用已解析对象生成键，避免再次解码。
	if bytes.Contains(body, []byte(`"prompt_cache_key"`)) {
		var probe map[string]any
		if err := jsonutil.Decode(body, &probe); err == nil && probe != nil {
			if existing, ok := probe["prompt_cache_key"].(string); ok && existing != "" {
				return body // 已有 key：不覆盖（保持既有语义）
			}
			// 键名出现但不是非空字符串（null / 空串 / 嵌套误匹配）。
			// 复用这次已解码的 probe 继续走后面逻辑，不要再 Decode 一遍：
			// 60 万 token 的 body 一次解码就要数毫秒，重复解码是纯浪费。
			return injectCacheKeyInto(probe, uid, conversationID, body)
		}
		// 解码失败 → 无法改写，原样返回（与完整路径的失败语义一致）。
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body
	}
	return injectCacheKeyInto(obj, uid, conversationID, body)
}

// injectCacheKeyInto 在已解码的对象上执行 cache key 注入并重新编码。
// 调用方保证 obj 非 nil；重编码失败时回落原始 body（不改写即视为未处理）。
func injectCacheKeyInto(obj map[string]any, uid, conversationID string, body []byte) []byte {
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 `wb2a-<uid8>-<convHex>` 格式的稳定 cache key。
//
// uid8 是可读标识；convHex = sha256(uid + "|" + conversationID)[:16] 的 hex 决定分桶。
// 同 UID、同会话源的键稳定；会话源为空时，同 UID 的所有空会话请求仍得到同一键。
// 完整 UID 参与哈希，不依赖 uid8 唯一，也不将缓存分桶等同于上游访问授权。
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return "wb2a-" + uid8 + "-" + convHex
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
