// ═══ 更新日志 ═══
// 2026-09-21：保护退回「命中即停止」：去掉写出闸门与可重发断言，改为断言命中后错误帧
// 如实写出、客户端不会收到成功终态，且正常正文/长推理仍实时透传。
// 2026-09-20：补正文（content）重复短行的检测回归：正文循环必须被识别并按正文侧
// 错误码中止；正常正文、长行、多短行必须原样透传。
// 2026-09-19：回归短行循环阈值、Unicode/CRLF分片、EOF/取消、模型范围及有界检测内存。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReasoningLoopGuardExactCRLFThresholdAcrossChunks(t *testing.T) {
	text := strings.Repeat(strings.Repeat("甲", 30)+"\r\n", 64) + strings.Repeat(strings.Repeat("乙", 29)+"\r\n", 192)
	runes := []rune(text)
	if len(runes) != 8000 {
		t.Fatal("fixture no longer reaches the exact character threshold")
	}
	for _, width := range []int{1, 2, 7, 31, 4096} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			guard := &reasoningLoopGuard{}
			var observed error
			for offset := 0; offset < len(runes); offset += width {
				end := offset + width
				if end > len(runes) {
					end = len(runes)
				}
				observed = guard.add(string(runes[offset:end]))
				if observed != nil {
					if end != len(runes) {
						t.Fatal("guard triggered before the exact threshold")
					}
					break
				}
			}
			if !IsReasoningLoopError(observed) || guard.characters != 8000 || guard.size != 256 {
				t.Fatalf("threshold mismatch: chars=%d size=%d err=%v", guard.characters, guard.size, observed)
			}
			if strings.Contains(observed.Error(), strings.Repeat("甲", 10)) {
				t.Fatal("guard error exposed reasoning text")
			}
		})
	}
}

func TestReasoningLoopGuardEOFCompletesLastLogicalLine(t *testing.T) {
	line := strings.Repeat("x", 32)
	guard := &reasoningLoopGuard{}
	if err := guard.add(strings.Repeat(line+"\n", 255) + line); err != nil {
		t.Fatalf("unterminated last line was counted too early: %v", err)
	}
	if guard.size != 255 || !IsReasoningLoopError(guard.finish()) {
		t.Fatalf("EOF did not complete the 256th line: size=%d", guard.size)
	}
}

func TestReasoningLoopGuardLongAndDiverseControlsRemainUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"long_single_line", strings.Repeat("这是正常的长行", 40000)},
		{"matrix_rows", strings.Repeat(strings.Repeat("0 ", 40)+"\n", 300)},
		{"sql_templates", strings.Repeat("INSERT INTO measurements(sensor_id, observed_value) VALUES (1, 0);\n", 300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := &reasoningLoopGuard{}
			if err := guard.add(tc.text); err != nil {
				t.Fatalf("long-line control rejected: %v", err)
			}
			if err := guard.finish(); err != nil {
				t.Fatal(err)
			}
			if guard.characters != utf8.RuneCountInString(tc.text) || cap(guard.line) > 4096 || guard.size > 256 || len(guard.counts) > 256 {
				t.Fatalf("detector lost characters or exceeded its memory bound: chars=%d cap=%d window=%d unique=%d", guard.characters, cap(guard.line), guard.size, len(guard.counts))
			}
		})
	}
	guard := &reasoningLoopGuard{}
	for index := 0; index < 2000; index++ {
		if err := guard.add(fmt.Sprintf("独立步骤 %d\n", index)); err != nil {
			t.Fatal(err)
		}
		if len(guard.counts) > 256 {
			t.Fatal("line identities grew beyond the window")
		}
	}
}

func reasoningGuardTestFrame(text string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": text}}}})
	return "data: " + string(raw) + "\n\n"
}

func TestReasoningLoopGuardModelScopeAndDisabledCompatibility(t *testing.T) {
	text := strings.Repeat(strings.Repeat("x", 32)+"\n", 300)
	raw := reasoningGuardTestFrame(text) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, model := range []string{"deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash", "sg:deepseek-v4.1-flash", " CN:DEEPSEEK-V4.1-FLASH "} {
		t.Run(model, func(t *testing.T) {
			result, err := Aggregate(strings.NewReader(raw), StreamOptions{Model: model, ReasoningLoopGuard: true})
			if result != nil || !IsReasoningLoopError(err) {
				t.Fatalf("explicit same-model alias was not guarded: %v", err)
			}
		})
	}
	for _, options := range []StreamOptions{
		{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: false},
		{Model: "deepseek-v3", ReasoningLoopGuard: true},
		{Model: "other:deepseek-v4.1-flash", ReasoningLoopGuard: true},
		{Model: "deepseek-v4.1-flash-other", ReasoningLoopGuard: true},
	} {
		result, err := Aggregate(strings.NewReader(raw), options)
		if err != nil || result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["reasoning_content"] != text {
			t.Fatalf("disabled/non-target guard changed output for %q: %v", options.Model, err)
		}
	}
	if _, err := Aggregate(strings.NewReader(raw)); err != nil {
		t.Fatalf("callers omitting StreamOptions lost compatibility: %v", err)
	}
}

type reasoningGuardCancelledReader struct{ source *strings.Reader }

func (r reasoningGuardCancelledReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if err == io.EOF {
		return n, context.Canceled
	}
	return n, err
}

func TestReasoningLoopGuardDoesNotTurnCancellationIntoEOFFinalization(t *testing.T) {
	line := strings.Repeat("x", 32)
	partial := reasoningGuardTestFrame(strings.Repeat(line+"\n", 255) + line)
	options := StreamOptions{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true}
	_, err := Aggregate(reasoningGuardCancelledReader{strings.NewReader(partial)}, options)
	if !errors.Is(err, context.Canceled) || IsReasoningLoopError(err) {
		t.Fatalf("cancellation was replaced by a loop result: %v", err)
	}
	rec := httptest.NewRecorder()
	err = Stream(rec, reasoningGuardCancelledReader{strings.NewReader(partial)}, options)
	if !errors.Is(err, context.Canceled) || IsReasoningLoopError(err) || strings.Contains(rec.Body.String(), ReasoningLoopErrorCode) {
		t.Fatalf("stream cancellation was replaced by loop detection: %v", err)
	}
}

// TestReasoningLoopGuardStreamStopsAndReports 验证「命中即停止」：循环命中时 Stream
// 把错误码如实写给客户端并结束该次请求，不再压住错误帧等待调用方重发。
func TestReasoningLoopGuardStreamStopsAndReports(t *testing.T) {
	text := strings.Repeat(strings.Repeat("x", 32)+"\n", 300)
	raw := reasoningGuardTestFrame(text) + "data: [DONE]\n\n"
	options := StreamOptions{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true}
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), options)
	if !IsReasoningLoopError(err) {
		t.Fatalf("guard did not trigger: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, ReasoningLoopErrorCode) {
		t.Fatalf("loop error was not delivered to the client: %d bytes", len(body))
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatal("loop stop did not close the stream with [DONE]")
	}
	// 命中即停止：不能给客户端一个成功终态，否则调用方会以为这一轮正常结束。
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal("loop stop emitted a successful finish")
	}
}

// TestReasoningLoopGuardStreamReleasesLongReasoning 验证长且多样的推理原样透传：
// 没有写出闸门之后，推理必须实时到达客户端，不能因为启用了保护而被压住。
func TestReasoningLoopGuardStreamReleasesLongReasoning(t *testing.T) {
	var unique strings.Builder
	for index := 0; index < 4000; index++ {
		fmt.Fprintf(&unique, "独立推理步骤 %04d\n", index)
	}
	raw := reasoningGuardTestFrame(unique.String()) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if err != nil {
		t.Fatalf("diverse reasoning was rejected: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "独立推理步骤") {
		t.Fatal("long diverse reasoning never reached the client")
	}
}

// TestReasoningLoopGuardStreamStopsLateLoop 复现线上观测到的循环形态：先有一段很长的
// 正常推理，之后才滑进重复短行。判定点在 18000–34000 字符之间，命中后必须中止并
// 按推理侧错误码如实回报。
func TestReasoningLoopGuardStreamStopsLateLoop(t *testing.T) {
	var text strings.Builder
	for index := 0; index < 2000; index++ {
		fmt.Fprintf(&text, "分析第 %04d 个独立步骤\n", index)
	}
	for index := 0; index < 400; index++ {
		text.WriteString("检查同一步骤\n")
	}
	if chars := utf8.RuneCountInString(text.String()); chars < reasoningLoopMinChars*3 {
		t.Fatalf("fixture no longer reproduces a late loop: %d characters", chars)
	}
	raw := reasoningGuardTestFrame(text.String()) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if !IsReasoningLoopError(err) {
		t.Fatalf("late loop was not stopped: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, ReasoningLoopErrorCode) {
		t.Fatalf("late loop was not reported to the client: %d bytes", len(body))
	}
}

func outputGuardTestFrame(text string) string {
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	return "data: " + string(raw) + "\n\n"
}

// TestOutputLoopGuardDetectsRepeatedContent 复现线上「疯狂输出」的真实形态：重复发生在
// 正文里（观测样本是 1864 行「我执行。」），推理侧几乎没有重复。旧实现只看
// reasoning_content，因此既不中断也不重试。
func TestOutputLoopGuardDetectsRepeatedContent(t *testing.T) {
	text := strings.Repeat("我执行。\n", 400)
	raw := outputGuardTestFrame(text) + "data: [DONE]\n\n"
	options := StreamOptions{Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true}
	result, err := Aggregate(strings.NewReader(raw), options)
	if result != nil || !IsLoopGuardError(err) {
		t.Fatalf("repeated content was not treated as a loop: %v", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || streamErr.Code != OutputLoopErrorCode {
		t.Fatalf("content loop used the wrong code: %+v", streamErr)
	}
	// 错误里只能有计数，不能回显用户正文。
	if strings.Contains(err.Error(), "我执行") {
		t.Fatal("content loop error exposed output text")
	}
}

// TestOutputLoopGuardStopsAndReportsContentLoop 验证正文循环命中即停止：错误帧按正文侧
// 错误码写给客户端，且不附带成功终态。
func TestOutputLoopGuardStopsAndReportsContentLoop(t *testing.T) {
	text := strings.Repeat("我执行。\n", 400)
	raw := outputGuardTestFrame(text) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if !IsLoopGuardError(err) {
		t.Fatalf("content loop was not stopped: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, OutputLoopErrorCode) {
		t.Fatalf("content loop was not reported to the client: %d bytes", len(body))
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatal("content loop stop emitted a successful finish")
	}
}

// TestOutputLoopGuardReleasesNormalContent 验证正常正文不被误判：散文、代码块、列表、
// 表格、单行长文本、短回复都必须原样透传，且不出现循环错误码。
func TestOutputLoopGuardReleasesNormalContent(t *testing.T) {
	var list strings.Builder
	for index := 0; index < 400; index++ {
		fmt.Fprintf(&list, "- 第 %d 项：说明文字各不相同\n", index)
	}
	for _, tc := range []struct{ name, text, needle string }{
		{"prose", "这是一段正常的中文回答，第一行就超过三十二个字符，因此正文闸门立即放行。", "这是一段正常的中文回答"},
		{"code_block", "```go\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```\n", "func main()"},
		{"list", list.String(), "- 第 0 项"},
		// 三行交替重复的表格：不同短行只有 3 个、覆盖率 100%，但没有任何一行占主导，
		// 正文侧必须放行，不能当成循环。
		{"table", strings.Repeat("| 字段 | 说明 |\n| --- | --- |\n| alpha | beta |\n", 200), "| alpha | beta |"},
		{"single_long_line", strings.Repeat("这是没有换行的长正文", 3000), "这是没有换行的长正文"},
		{"short_reply", "好的。", "好的。"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := outputGuardTestFrame(tc.text) + "data: [DONE]\n\n"
			rec := httptest.NewRecorder()
			err := Stream(rec, strings.NewReader(raw), StreamOptions{
				Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
			if err != nil {
				t.Fatalf("normal content was rejected: %v", err)
			}
			// 帧是 JSON，正文里的换行会被转义，因此断言用一个不含换行的片段。
			if !strings.Contains(rec.Body.String(), tc.needle) {
				t.Fatal("normal content never reached the client")
			}
			if strings.Contains(rec.Body.String(), OutputLoopErrorCode) {
				t.Fatal("normal content was reported as a loop")
			}
		})
	}
}

// TestOutputLoopGuardIgnoresNonTargetModel 正文保护同样只在目标模型上生效，其他模型
// 的重复正文必须原样透传（保护是启发式的，不应波及未观测到问题的模型）。
func TestOutputLoopGuardIgnoresNonTargetModel(t *testing.T) {
	text := strings.Repeat("我执行。\n", 400)
	raw := outputGuardTestFrame(text) + "data: [DONE]\n\n"
	for _, model := range []string{"deepseek-v3", "cn:glm-9.9", "other:deepseek-v4.1-flash"} {
		result, err := Aggregate(strings.NewReader(raw), StreamOptions{Model: model, ReasoningLoopGuard: true})
		if err != nil {
			t.Fatalf("non-target model %q was guarded: %v", model, err)
		}
		if result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != text {
			t.Fatalf("non-target model %q lost content", model)
		}
	}
}

// TestOutputLoopGuardPassesAmbiguousShortLines 验证「两行交替重复」不会被误截：每行
// 8 个字符、只有两种短行交替，没有任何一行占多数（各 50%，低于正文侧的 75% 门槛），
// 必须原样透传。这条边界不能因为退回单项停止而放松。
func TestOutputLoopGuardPassesAmbiguousShortLines(t *testing.T) {
	var text strings.Builder
	for index := 0; index < 200; index++ {
		if index%2 == 0 {
			text.WriteString("思考中\n")
		} else {
			text.WriteString("等待中\n")
		}
	}
	raw := outputGuardTestFrame(text.String()) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if err != nil {
		t.Fatalf("ambiguous but non-looping content was rejected: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "思考中") || strings.Contains(body, OutputLoopErrorCode) {
		t.Fatalf("ambiguous short lines were mishandled: %d bytes", len(body))
	}
}

// TestReasoningLoopGuardStopsThroughTinyFrames 复现线上真实的碎帧形态：上游把推理切成
// 约 220 字节的小帧、每帧只带一两个字符（实测 124–222 字节/字符）。检测按字符与行计数，
// 不受分帧粒度影响：两万多字符处滑进重复短行后，必须命中并按推理侧错误码中止。
func TestReasoningLoopGuardStopsThroughTinyFrames(t *testing.T) {
	var payload strings.Builder
	// 前半：32000 个单字符帧，拼成一条长行，永远不会被计为重复，不会提前触发判定。
	for index := 0; index < 32000; index++ {
		payload.WriteString(reasoningGuardTestFrame(string(rune('a' + index%26))))
	}
	// 后半：重复短行「x」，凑满 256 行窗口后重复覆盖达标（每行两个帧：内容 + 换行）。
	for index := 0; index < 400; index++ {
		payload.WriteString(reasoningGuardTestFrame("x"))
		payload.WriteString(reasoningGuardTestFrame("\n"))
	}
	raw := payload.String() + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if !IsLoopGuardError(err) {
		t.Fatalf("tiny-frame loop was not stopped: %v", err)
	}
	if body := rec.Body.String(); !strings.Contains(body, ReasoningLoopErrorCode) {
		t.Fatalf("tiny-frame loop was not reported to the client: %d bytes", len(body))
	}
}
