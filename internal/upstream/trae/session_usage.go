// session_usage.go — 从商业用量接口取「本轮真实扣费」。
//
// ═══ 更新日志 ═══
// 2026-11-15：排除空/错轮次账单；保留商业缓存计数和有效性，并校验分组归因。
//
// ═══ 为什么需要这一步（2026-10-18 实测）═══
//
// Trae 的 `llm_utils_chat` 事件流里，`token_usage` 事件**只有 token 数，没有
// credit 字段**（原始事件逐字确认）：
//
//	event:token_usage
//	data:{"prompt_tokens":17,"completion_tokens":21,"total_tokens":38,
//	      "cache_read_input_tokens":0,"reasoning_tokens":17,"cluster":"normal_context"}
//
// 于是网关的账本永远拿不到扣费，面板「积分」列恒为「—」，日志刷
// `WARN: 上游返回用量但无扣费`。成本账本（NoteModelCost）也因此从不更新，
// 选号无法按「免费/便宜优先」排序。
//
// 官方客户端走的是另一个端点取真实账单：
//
//	POST {gateway}/api/v1/commercial/get_session_usage
//	{"session_id": "<turn id>"}
//
// 实测（2026-10-18，dev 真实上游）返回：
//
//	{"user_usage_group_by_session":{
//	   "amount_float":0.07,"credits_float":0.07,
//	   "extra_info":{"input_token":17,"output_token":84},
//	   "model_name":"GLM-5.3","use_max_mode":false,
//	   "session_id":"creditprobe-1792276415060161781"}}
//
// 两个关键事实：
//  1. **turn id 就是我们发出去的 request_id**（上游 metadata 事件原样回显），
//     不需要额外的会话句柄；
//  2. **没有结算延迟**——连续三次轮询（间隔 2s）拿到的值完全一致，首次即可用。
//     所以不需要参考实现那套指数退避轮询。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// SessionUsagePath 商业用量端点（与 llm_utils_chat 同网关主机）。
const SessionUsagePath = "/api/v1/commercial/get_session_usage"

// sessionCreditTimeout 查账超时。
//
// 这个查询发生在**流尾**（usage 帧之前），超时会推迟客户端的 [DONE]。
// 实测同一主机一次往返 <200ms，取 5s 留足余量又不至于让坏网络拖住整轮。
const sessionCreditTimeout = 5 * time.Second

const maxSessionExactTokens int64 = 1<<53 - 1

// SessionCacheCount 是商业账单自己的整数观测。
// Present 区分字段缺失；Valid=false 时 Value 不代表实际零值。
type SessionCacheCount struct {
	Value   int64
	Present bool
	Valid   bool
}

// SessionCredit 是本轮的真实扣费观测。
type SessionCredit struct {
	// SessionMatched 只在账单回显的 session_id 与请求轮次精确一致时为真。
	// 不保存原始标识；字段缺失时仍可读扣费，但不能据此把账单缓存关联到 SSE。
	SessionMatched bool
	// CacheAttributionSafe 表示关联和账单分组未见矛盾；仍须校验总计并对齐 SSE。
	CacheAttributionSafe bool
	// Credits 实际扣费（credits_float；缺失时退回 amount_float）。
	Credits float64
	// Amount 账单金额形态（amount_float）。
	Amount float64
	// InputTokens / OutputTokens 是账单口径，可能汇总同一轮的多次调用。
	// 严格对齐时必须检查 Present/Valid，不能把缺失或非法值的默认零当作真实零。
	InputTokens         int64
	OutputTokens        int64
	InputTokensPresent  bool
	InputTokensValid    bool
	OutputTokensPresent bool
	OutputTokensValid   bool
	// 账单可能汇总多次调用；不能未经轮次关联和口径对齐就覆盖单条 SSE。
	CacheReadTokens  SessionCacheCount
	CacheWriteTokens SessionCacheCount
	// ModelName 上游记账的模型显示名（如 "GLM-5.3"）。
	ModelName string
	// UseMaxMode is the billing service's reported flag. It is not sufficient
	// to infer the requested tier: observed __max responses can still bill false.
	UseMaxMode bool
}

// FetchSessionCredit 用 turn id 查本轮扣费。
//
// turnID：legacy 路径传本次请求 body.session_id（每次独立，禁止会话复用）；
// Remote 路径传建会话拿到的用户 message_id。
//
// 返回 ok=false 的以下情形（都**不**是错误，只是没观测到）：
//   - 上游还没出账单（分组缺失、空对象，或无身份/实际用量的默认零值结构）；
//   - 账单回显了其它轮次的标识，或标识类型错误；
//   - 字段形态变化（credits_float 非数值）；
//   - 网络/HTTP 失败（记一条 WARN 后返回）。
//
// 缺失≠0：调用方只在 ok=true 时写账本，避免把「没查到」记成「免费」。
func FetchSessionCredit(ctx context.Context, a *auth.Auth, turnID string) (SessionCredit, bool) {
	var out SessionCredit
	turnID = trimSpaceASCII(turnID)
	if turnID == "" || a == nil {
		return out, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, sessionCreditTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]string{"session_id": turnID})
	if err != nil {
		return out, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		GatewayBaseVar+SessionUsagePath, bytes.NewReader(body))
	if err != nil {
		return out, false
	}
	// 头族与模型网关一致（Cloud-IDE-JWT），但只需要最小集合：实测
	// Authorization + Content-Type 即可，多带的 x-lscbd-* 不影响结果。
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		log.Printf("WARN: [trae] 查扣费失败（turn=%s）：%v", turnID, err)
		return out, false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		log.Printf("WARN: [trae] 查扣费 HTTP %d（turn=%s）：%s",
			resp.StatusCode, turnID, truncate(string(raw), 200))
		return out, false
	}
	var payload struct {
		Group map[string]any `json:"user_usage_group_by_session"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// 缓存计数不能先转 float64，否则安全整数边界附近的小数会被舍入成整数。
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		log.Printf("WARN: [trae] 查扣费响应解析失败（turn=%s）：%v", turnID, err)
		return out, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return out, false
	}
	if payload.Group == nil {
		return out, false
	}
	if value, present := payload.Group["session_id"]; present && value != nil {
		id, ok := value.(string)
		if !ok || id != "" && id != turnID {
			return out, false
		}
		out.SessionMatched = id == turnID
	}
	credit, ok := usageFloat(payload.Group["credits_float"])
	if !ok {
		// 有的账号只给 amount_float；两者都缺就是没出账单。
		if credit, ok = usageFloat(payload.Group["amount_float"]); !ok {
			return out, false
		}
	}
	out.Credits = credit
	if amount, ok := usageFloat(payload.Group["amount_float"]); ok {
		out.Amount = amount
	}
	if extra, ok := payload.Group["extra_info"].(map[string]any); ok {
		input := sessionBillCount(extra, "input_token")
		output := sessionBillCount(extra, "output_token")
		out.InputTokens, out.InputTokensPresent, out.InputTokensValid = input.Value, input.Present, input.Valid
		out.OutputTokens, out.OutputTokensPresent, out.OutputTokensValid = output.Value, output.Present, output.Valid
		out.CacheReadTokens = sessionBillCount(extra, "cache_read_token")
		out.CacheWriteTokens = sessionBillCount(extra, "cache_write_token")
	}
	out.ModelName, _ = payload.Group["model_name"].(string)
	if flag, ok := payload.Group["use_max_mode"].(bool); ok {
		out.UseMaxMode = flag
	}
	if credit == 0 && !hasSessionBillEvidence(payload.Group) {
		return SessionCredit{}, false
	}
	out.CacheAttributionSafe = sessionCacheAttributionSafe(payload.Group, out.SessionMatched)
	return out, true
}

// 分组校验不负责判断账单与某条 SSE 是否同口径；调用方仍须严格匹配输入输出。
func sessionCacheAttributionSafe(group map[string]any, sessionMatched bool) bool {
	if !sessionMatched {
		return false
	}
	value, present := group["usage_group_details"]
	if !present {
		return true
	}
	details, ok := value.([]any)
	if !ok || len(details) != 1 {
		return false
	}
	detail, ok := details[0].(map[string]any)
	if !ok {
		return false
	}
	if value, present := detail["model_display_name"]; present {
		name, valid := value.(string)
		expected, rootValid := group["model_name"].(string)
		if !valid || !rootValid || name != expected {
			return false
		}
	}
	if value, present := detail["extra_info"]; present {
		extra, valid := value.(map[string]any)
		if !valid {
			return false
		}
		rootExtra, _ := group["extra_info"].(map[string]any)
		for _, key := range []string{"input_token", "output_token", "cache_read_token", "cache_write_token"} {
			if _, provided := extra[key]; !provided {
				continue
			}
			count := sessionBillCount(extra, key)
			total := sessionBillCount(rootExtra, key)
			if !count.Present || !count.Valid || !total.Present || !total.Valid || count.Value != total.Value {
				return false
			}
		}
	}
	return true
}

func sessionBillCount(extra map[string]any, key string) SessionCacheCount {
	value, present := extra[key]
	out := SessionCacheCount{Present: present}
	if count, ok := usageCount(value); present && ok && count <= maxSessionExactTokens {
		out.Value, out.Valid = count, true
	}
	return out
}

// 默认空账单与免费账单都可能返回 credits=0；身份或实际用量才能区分二者。
func hasSessionBillEvidence(group map[string]any) bool {
	for _, key := range []string{"session_id", "model_name", "model_display_name"} {
		if value, ok := group[key].(string); ok && trimSpaceASCII(value) != "" {
			return true
		}
	}
	if extra, ok := group["extra_info"].(map[string]any); ok {
		for _, key := range []string{"input_token", "output_token", "cache_read_token", "cache_write_token"} {
			if count := sessionBillCount(extra, key); count.Valid && count.Value > 0 {
				return true
			}
		}
	}
	if details, ok := group["usage_group_details"].([]any); ok {
		for _, value := range details {
			if detail, ok := value.(map[string]any); ok && hasSessionBillEvidence(detail) {
				return true
			}
		}
	}
	return false
}

// usageFloat 把 JSON 数值转 float64（兼容 float64 / json.Number / 原生整数）。
func usageFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// trimSpaceASCII 去掉首尾空白（turn id 只可能是 ASCII）。
func trimSpaceASCII(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// creditFetcher 是流尾查扣费的回调（translator 用）。
//
// 返回 ok=false 表示「没观测到」，此时不写 usage.credit——缺失≠0。
type creditFetcher func() (SessionCredit, bool)

// creditFor 构造一个绑定账号与 turn id 的回调。
func creditFor(a *auth.Auth, turnID string) creditFetcher {
	if a == nil || trimSpaceASCII(turnID) == "" {
		return nil
	}
	return func() (SessionCredit, bool) {
		return FetchSessionCredit(context.Background(), a, turnID)
	}
}

// creditLog 打印一次扣费观测（用于确认「默认 max」是否真的按 max 计费）。
func creditLog(scope, turnID string, credit SessionCredit) {
	note := ""
	if credit.UseMaxMode {
		note = " [max 档]"
	}
	log.Printf("INFO: [trae] %s 扣费 %.4f（模型=%s%s turn=%s 输入=%d 输出=%d）",
		scope, credit.Credits, credit.ModelName, note, turnID,
		credit.InputTokens, credit.OutputTokens)
}

// remoteCreditSpec 组装 Remote 路径的查账规格（turn id = 建会话拿到的 message_id）。
//
// 为什么用 message_id 而不是 chat_session_id：商业接口按**单轮消息**记账，
// chat_session_id 是一整个会话（多轮累加）。参考实现同样传 message_id
// （main.py 的 _bind_usage_turn(message_id)）。
func remoteCreditSpec(a *auth.Auth, messageID string, selection outboundSelection) *creditSpec {
	fetcher := creditFor(a, messageID)
	if fetcher == nil {
		return nil
	}
	return &creditSpec{scope: "Remote", turnID: messageID, fetcher: fetcher, selection: selection}
}
