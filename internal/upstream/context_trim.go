// ═══ 更新日志 ═══
// 2026-09-23：新增上下文超限时的最旧历史裁剪，供上游 11115 自动重发。
// context_trim.go 上游「prompt is too long」的兜底裁剪。
//
// ── 为什么需要这一层（实测依据） ──
//
// 上游 deepseek 系模型对 global 域的真实硬墙是 1,048,576（1 MiB），而模型元数据
// 声明的 context_length / max_allowed_size 是 1,000,000。客户端的自动压缩阈值取
// min(auto_compact_token_limit, 9/10 × context_window)，本身低于硬墙；但客户端的
// token 估算与上游预检口径不同，实测会出现「客户端认为 664,841、上游预检 1,048,691」
// 的偏差，于是压缩还没来得及触发，请求已经被拒。
//
// 实测失败样本都只超出硬墙几十到几百 token（1,048,684 / 1,048,691 / 1,048,868 /
// 1,048,892 / 1,049,388），属于「差一点点」，此时丢掉最旧的一段历史就能通过，
// 没必要让调用方看到 400 并被迫手动开新会话。
//
// ── 为什么按「轮」丢而不是按条丢 ──
//
// chat 协议里 assistant 的 tool_calls 与其后的 tool 结果必须成对出现，缺一半上游会
// 返 400（网关已有 cleanupOrphanToolCalls 兜脏数据，但不该由裁剪主动制造）。按轮
// （user → 下一个 user 之前的全部消息）丢弃时，tool_calls 与结果落在同一轮内，配对
// 关系天然完整；系统消息作为前导段始终保留。
package upstream

import (
	"encoding/json"
	"math"

	"workbuddy2api/internal/jsonutil"
)

// contextTrimKeepRatios 逐级裁剪时保留的「轮」比例。
//
// 索引即裁剪档位：每次重发前取下一档，丢得更多一点。三档足够覆盖实测的「差一点点」
// 场景；仍然超限就如实把上游错误交给调用方，不在这里伪造成功。
var contextTrimKeepRatios = []float64{0.75, 0.5, 0.25}

// ContextTrimLevels 返回裁剪档位数，供调用方判断还能不能再重发。
func ContextTrimLevels() int { return len(contextTrimKeepRatios) }

// TrimOldestContext 丢掉最旧的一部分「轮」，保留系统前导段与最新的若干轮。
//
// keepRatio 为要保留的轮比例（0,1]；不足 2 轮、body 不是 JSON 对象、或裁剪后没有
// 变小时返回 false，调用方据此放弃重发。返回的 body 只做「删除整轮」，不修改任何
// 保留消息的内容与字段。
func TrimOldestContext(body []byte, keepRatio float64) ([]byte, bool) {
	if len(body) == 0 || keepRatio <= 0 || keepRatio >= 1 {
		return body, false
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}

	// 收集每一轮的开始下标（role == user）。前导的系统/开发者消息不属于任何一轮。
	var turnStarts []int
	for i, raw := range msgs {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role == "user" {
			turnStarts = append(turnStarts, i)
		}
	}
	if len(turnStarts) < 2 {
		return body, false
	}

	keepTurns := int(math.Ceil(float64(len(turnStarts)) * keepRatio))
	if keepTurns < 1 {
		keepTurns = 1
	}
	dropTurns := len(turnStarts) - keepTurns
	if dropTurns < 1 {
		return body, false
	}

	cut := turnStarts[dropTurns]
	preamble := turnStarts[0]
	kept := make([]any, 0, preamble+len(msgs)-cut)
	kept = append(kept, msgs[:preamble]...)
	kept = append(kept, msgs[cut:]...)
	if len(kept) >= len(msgs) {
		return body, false
	}
	obj["messages"] = kept

	out, err := json.Marshal(obj)
	if err != nil || len(out) >= len(body) {
		return body, false
	}
	return out, true
}
