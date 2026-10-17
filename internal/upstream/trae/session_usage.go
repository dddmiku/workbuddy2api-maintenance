// session_usage.go — 从商业用量接口取「本轮真实扣费」。
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

// SessionCredit 是本轮的真实扣费观测。
type SessionCredit struct {
	// Credits 实际扣费（credits_float；缺失时退回 amount_float）。
	Credits float64
	// Amount 金额形态（cost_money_float）。
	Amount float64
	// InputTokens / OutputTokens 上游记账用的 token 数（与事件流可能略有出入：
	// 这里含缓存写入等内部项）。
	InputTokens  int64
	OutputTokens int64
	// ModelName 上游记账的模型显示名（如 "GLM-5.3"）。
	ModelName string
	// UseMaxMode 上游是否按 1M 档计费——「默认走 max」是否真的生效，看这个字段。
	UseMaxMode bool
}

// FetchSessionCredit 用 turn id 查本轮扣费。
//
// turnID：legacy 路径传我们自己生成的 request_id（上游 metadata 原样回显）；
// Remote 路径传建会话拿到的 message_id。
//
// 返回 ok=false 的三种情形（都**不**是错误，只是没观测到）：
//   - 上游还没出账单（user_usage_group_by_session 缺失或为空）；
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
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("WARN: [trae] 查扣费响应解析失败（turn=%s）：%v", turnID, err)
		return out, false
	}
	if payload.Group == nil {
		return out, false
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
		if v, ok := usageFloat(extra["input_token"]); ok {
			out.InputTokens = int64(v)
		}
		if v, ok := usageFloat(extra["output_token"]); ok {
			out.OutputTokens = int64(v)
		}
	}
	out.ModelName, _ = payload.Group["model_name"].(string)
	if flag, ok := payload.Group["use_max_mode"].(bool); ok {
		out.UseMaxMode = flag
	}
	return out, true
}

// usageFloat 把 JSON 数值转 float64（兼容 float64 / json.Number / 字符串数字）。
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
func remoteCreditSpec(a *auth.Auth, messageID string) *creditSpec {
	fetcher := creditFor(a, messageID)
	if fetcher == nil {
		return nil
	}
	return &creditSpec{scope: "Remote", turnID: messageID, fetcher: fetcher}
}
