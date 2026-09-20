// ═══ 更新日志 ═══
// 2026-09-20：补正文（content）重复短行的检测与写出闸门回归：正文循环必须在客户端
//
//	零字节时被按住并标记可重发；正常正文、长行、多短行必须实时放行。
//
// 2026-09-19：回归短行循环阈值、Unicode/CRLF分片、EOF/取消、模型范围及有界检测内存。
// 2026-09-20：补「压制期命中→客户端零字节且错误可重发」与「长推理按字符上限放行」两条回归。
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
	"time"
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

// TestReasoningLoopGuardStreamHoldsBackUntilRetryDecision 验证写出闸门：命中循环时
// Stream 在 LoopRetryAvailable 下不向客户端写任何字节，并把错误标成 Retryable，
// 让 handler 能在同一账号上整段重发而不让用户看到半截推理。
func TestReasoningLoopGuardStreamHoldsBackUntilRetryDecision(t *testing.T) {
	text := strings.Repeat(strings.Repeat("x", 32)+"\n", 300)
	raw := reasoningGuardTestFrame(text) + "data: [DONE]\n\n"
	options := StreamOptions{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true}
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), options)
	if !IsReasoningLoopError(err) {
		t.Fatalf("guard did not trigger: %v", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || !streamErr.Retryable {
		t.Fatalf("loop error was not marked retryable: %+v", streamErr)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client saw %d bytes before the retry decision: %q", len(body), body)
	}

	// 没有重发额度时必须如实回报，不能既不给内容也不给错误。
	rec = httptest.NewRecorder()
	err = Stream(rec, strings.NewReader(raw), StreamOptions{Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if !IsReasoningLoopError(err) || strings.Contains(rec.Body.String(), ReasoningLoopErrorCode) == false {
		t.Fatalf("exhausted retries did not surface the loop error: %v", err)
	}
}

// TestReasoningLoopGuardStreamReleasesLongReasoning 验证长推理不会因为保护而无限压制：
// 纯推理超过压制上限仍未命中时立即放行，客户端能实时看到这段推理。
func TestReasoningLoopGuardStreamReleasesLongReasoning(t *testing.T) {
	var unique strings.Builder
	for index := 0; index < 4000; index++ {
		fmt.Fprintf(&unique, "独立推理步骤 %04d\n", index)
	}
	raw := reasoningGuardTestFrame(unique.String()) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	if err != nil {
		t.Fatalf("diverse reasoning was rejected: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "独立推理步骤") {
		t.Fatal("long diverse reasoning never reached the client")
	}
}

// TestReasoningLoopGuardStreamHoldsLateLoop 复现线上观测到的循环形态：先有一段很长的
// 正常推理，之后才滑进重复短行。判定点在 18000–34000 字符之间，压制必须能撑到那时，
// 否则客户端先看到重复文本、重发就失去意义。
func TestReasoningLoopGuardStreamHoldsLateLoop(t *testing.T) {
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
		Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	var streamErr *StreamError
	if !IsReasoningLoopError(err) || !errors.As(err, &streamErr) || !streamErr.Retryable {
		t.Fatalf("late loop was not held back for retry: %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client saw %d bytes before the late-loop retry decision", len(body))
	}
}

// reasoningGuardSlowReader 每次只交付一小段，并在片段之间停顿，用来复现「想得慢、
// 每次只吐几个字」的流。
type reasoningGuardSlowReader struct {
	chunks []string
	delay  time.Duration
	index  int
}

func (r *reasoningGuardSlowReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

// TestReasoningLoopGuardStreamReleasesOnTimeout 验证时间上限：推理量远没到字符上限、
// 却迟迟没有新内容时，客户端不能一直等下去，到点必须放行并转为实时透传。
func TestReasoningLoopGuardStreamReleasesOnTimeout(t *testing.T) {
	previous := ReasoningLoopHoldBackTimeout
	ReasoningLoopHoldBackTimeout = 40 * time.Millisecond
	t.Cleanup(func() { ReasoningLoopHoldBackTimeout = previous })

	text := strings.Repeat("还在想\n", 40)
	raw := reasoningGuardTestFrame(text) + "data: [DONE]\n\n"
	chunks := make([]string, 0, len(raw))
	for offset := 0; offset < len(raw); offset += 8 {
		end := offset + 8
		if end > len(raw) {
			end = len(raw)
		}
		chunks = append(chunks, raw[offset:end])
	}
	rec := httptest.NewRecorder()
	err := Stream(rec, &reasoningGuardSlowReader{chunks: chunks, delay: 15 * time.Millisecond}, StreamOptions{
		Model: "deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	if err != nil {
		t.Fatalf("slow but valid reasoning was rejected: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "还在想") {
		t.Fatal("timeout did not release the held-back reasoning")
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

// TestOutputLoopGuardHoldsContentUntilRetryDecision 验证正文循环也能整段重发：
// 命中时客户端必须还是零字节，错误标记 Retryable 供 handler 同账号重发。
func TestOutputLoopGuardHoldsContentUntilRetryDecision(t *testing.T) {
	text := strings.Repeat("我执行。\n", 400)
	raw := outputGuardTestFrame(text) + "data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(raw), StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	var streamErr *StreamError
	if !IsLoopGuardError(err) || !errors.As(err, &streamErr) || !streamErr.Retryable {
		t.Fatalf("content loop was not held back for retry: %v", err)
	}
	if body := rec.Body.String(); body != "" {
		t.Fatalf("client saw %d bytes before the retry decision: %q", len(body), body)
	}

	// 没有重发额度时必须如实回报，不能既不给内容也不给错误。
	rec = httptest.NewRecorder()
	err = Stream(rec, strings.NewReader(raw), StreamOptions{Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true})
	if !IsLoopGuardError(err) || !strings.Contains(rec.Body.String(), OutputLoopErrorCode) {
		t.Fatalf("exhausted retries did not surface the content loop: %v", err)
	}
}

// TestOutputLoopGuardReleasesNormalContent 验证正文闸门不会拖慢正常回答：散文、代码块、
// 列表、表格、单行长文本都必须在第一帧之后立刻放行。
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
				Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
			if err != nil {
				t.Fatalf("normal content was rejected: %v", err)
			}
			// 帧是 JSON，正文里的换行会被转义，因此断言用一个不含换行的片段。
			if !strings.Contains(rec.Body.String(), tc.needle) {
				t.Fatal("normal content never reached the client")
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

// TestOutputLoopGuardReleasesOnContentTimeout 验证正文压制期有独立的时间上限：一段
// 没有换行、始终只有一两种短行的输出既不可能构成短行循环，也不该被压到推理侧的
// 60 秒上限。到点必须放行，客户端能看到这段正文。
func TestOutputLoopGuardReleasesOnContentTimeout(t *testing.T) {
	previous := ContentLoopHoldBackTimeout
	ContentLoopHoldBackTimeout = 40 * time.Millisecond
	t.Cleanup(func() { ContentLoopHoldBackTimeout = previous })

	// 每行 8 个字符、只有两种短行交替：不会命中（没有占多数的一行），也不会因为长行、
	// 空行或第三种短行而放行，只能靠时间上限出去。
	var text strings.Builder
	for index := 0; index < 200; index++ {
		if index%2 == 0 {
			text.WriteString("思考中\n")
		} else {
			text.WriteString("等待中\n")
		}
	}
	raw := outputGuardTestFrame(text.String()) + "data: [DONE]\n\n"
	chunks := make([]string, 0, len(raw))
	for offset := 0; offset < len(raw); offset += 64 {
		end := offset + 64
		if end > len(raw) {
			end = len(raw)
		}
		chunks = append(chunks, raw[offset:end])
	}
	rec := httptest.NewRecorder()
	err := Stream(rec, &reasoningGuardSlowReader{chunks: chunks, delay: 8 * time.Millisecond}, StreamOptions{
		Model: "global:deepseek-v4.1-flash", ReasoningLoopGuard: true, LoopRetryAvailable: true})
	if err != nil {
		t.Fatalf("ambiguous but non-looping content was rejected: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "思考中") {
		t.Fatal("content timeout did not release the held-back content")
	}
}
