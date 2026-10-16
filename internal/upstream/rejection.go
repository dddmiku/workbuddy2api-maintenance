// ═══ 更新日志 ═══
// 2026-10-15：初版——把上游在流内发来的**明确拒绝**（带业务码）与「偶发掐流」区分开。
//
// 为什么需要它：通用 SSE 读取器此前把上游的 error 事件统一归成
// `upstream_read_error`，于是 handler 把「上游确定性地拒绝了这个请求」
// 当成「上游偶发断流」，换号重试三次（必然全败，换号改变不了请求体）后报 502。
// 业务码在重新包装时丢失，客户端与日志都看不到真正原因。
//
// 分界线是**可重试性**，不是错误严重程度：
//   - upstream_read_error / upstream_parse：传输层偶发，换号重发有意义；
//   - upstream_error（本文件识别的那类）：上游读懂了请求并明确说不，
//     重试与换号都无意义，只能如实回报并让调用方改请求。
package upstream

import (
	"errors"
	"strconv"
	"strings"

	"workbuddy2api/internal/upstreamerr"
)

// PromptTooLongCode 是上游对「输入超过上下文窗口」的拒绝码。
//
// 与 11115 / context_length_exceeded（另一套口径，见 context_error.go）不同：
// 4022 是 Trae 模型网关在**流内**发的，文案是
// "We're sorry, your prompt tokens have exceeded the maximum limit."。
// 实测边界就是模型窗口本身（llm_utils_chat 上约 1M token）。
const PromptTooLongCode = 4022

// UpstreamRejection 是上游对本次请求的确定性拒绝。
type UpstreamRejection struct {
	// Code 上游业务码；0 表示上游只给了文案没给码。
	Code int
	// Message 上游原文。
	Message string
}

// Detail 返回面向客户端的说明（保留上游原文，必要时补充可操作的解释）。
func (r *UpstreamRejection) Detail() string {
	if r == nil {
		return ""
	}
	// 4022 是「prompt tokens 超过上限」——确定性拒绝，且**换号/重试都无效**
	// （窗口是模型属性，与账号无关）。实测（2026-10-15，llm_utils_chat）：
	// prompt_tokens 1,025,078 成功、约 1.39M 触发 4022，边界即 1M 上下文窗口。
	// 照原文回「exceeded the maximum limit」看不出该做什么，所以补上真正的处置。
	if r.Code == PromptTooLongCode {
		detail := r.Message
		if detail != "" {
			detail += "；"
		}
		return detail + "本次输入已超过模型上下文窗口（约 1M token），请压缩会话历史或减少工具定义后重试；换账号无效"
	}
	// 3004 的文案是「超出速率限制」，但实测触发条件是**工具交互轮次**（不是体积，
	// 也不是调用频次）：2 条消息/1600KB 纯文本成功，而 120 条工具密集消息/311KB 被拒；
	// 只丢 tool_calls 或只丢 tool_call_id 仍被拒，两者都转成纯文本才通过。
	// 照原文回「稍后再试」会把调用方引向错误的自救方向（等一会、换个号都没用），
	// 所以这里补一句真正的处置方式。
	//
	// 措辞**不得声称「已回落」**（2026-10-16 修）：带图请求会主动放弃回落（Remote
	// 读不到内联图片，回落等于把图悄悄丢掉），此时说「已自动回落」是假的——用户按
	// 提示去「减少会话历史」也解决不了，因为真正的原因是本轮带了图片。文案只描述
	// 原因与两条出路，不替调用方断定网关做了什么。
	if r.Code == upstreamerr.RateLimitCode {
		detail := r.Message
		if detail != "" {
			detail += "；"
		}
		return detail + "上游对单次请求的工具交互轮次有限制（与体积无关）。" +
			"网关会优先走 Remote 会话端点绕开该限制；若仍看到本条，说明请求走了另一条路径" +
			"（本轮带图时只能走该路径，因为 Remote 读不到内联图片）。" +
			"处置：减少会话历史，或本轮去掉图片，或改用支持图片的模型后重试"
	}
	if r.Message != "" {
		return r.Message
	}
	return "upstream rejected the request"
}

// StreamUpstreamRejection 报告该错误是否为上游在流内发来的明确拒绝，是则返回其业务码与原文。
//
// 判据是 StreamError.Code == "upstream_error"，而不是去匹配文案：文案会随上游改版漂移，
// 拿它做分支是脆的。注意 3004 这类码在流内与 HTTP 失败两条路径上都会出现——
// HTTP 层的那条由 ContextTooLongHTTPDetail 之类的入口单独处理，本函数只管流内。
func StreamUpstreamRejection(err error) *UpstreamRejection {
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		return nil
	}
	if streamErr.Code != "upstream_error" {
		return nil
	}
	rejection := &UpstreamRejection{Message: streamErr.Message}
	if streamErr.Upstream != nil {
		if message, ok := streamErr.Upstream["message"].(string); ok && message != "" {
			rejection.Message = message
		}
		if code, ok := upstreamCodeValue(streamErr.Upstream["code"]); ok {
			rejection.Code = code
		}
	}
	return rejection
}

// upstreamCodeValue 兼容上游把业务码写成数字或字符串两种形态。
func upstreamCodeValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		code, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, false
		}
		return code, true
	}
	return 0, false
}
