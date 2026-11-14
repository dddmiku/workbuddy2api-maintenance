// ═══ 更新日志 ═══
// 2026-11-15：验证 Trae 即时交付正常载荷、保留循环检测与明确失败终态。
package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// Read boundaries let each assertion run after the preceding complete SSE event
// was consumed, before the next upstream event is available. No timing sleeps or
// concurrent access to ResponseRecorder are needed to prove early delivery.
type immediateProgressStep struct {
	raw    string
	before func()
}

type immediateProgressReader struct {
	steps   []immediateProgressStep
	index   int
	pending *strings.Reader
}

func (r *immediateProgressReader) Read(p []byte) (int, error) {
	for r.index < len(r.steps) {
		if r.pending == nil {
			step := r.steps[r.index]
			if step.before != nil {
				step.before()
			}
			r.pending = strings.NewReader(step.raw)
		}
		n, err := r.pending.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != io.EOF {
			return n, err
		}
		r.pending = nil
		r.index++
	}
	return 0, io.EOF
}

func immediateProgressFrame(delta map[string]any, finish string) string {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{choice}})
	return "data: " + string(raw) + "\n\n"
}

func immediateProgressOptions(enabled, retry bool) StreamOptions {
	return StreamOptions{
		Model: "trae:deepseek-v4.1-flash", ReasoningLoopGuard: true,
		LoopRetryAvailable: retry, ImmediateProgress: enabled,
	}
}

func immediateProgressAssertFailure(t *testing.T, raw string) {
	t.Helper()
	errorsSeen, doneSeen, finishes := 0, 0, 0
	err := readSSE(strings.NewReader(raw), func(event sseEvent) (bool, error) {
		if event.data == "[DONE]" {
			doneSeen++
			return false, nil
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(event.data), &frame); err != nil {
			return false, err
		}
		if frame["error"] != nil {
			errorsSeen++
		}
		choices, _ := frame["choices"].([]any)
		for _, rawChoice := range choices {
			choice := rawChoice.(map[string]any)
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				finishes++
			}
		}
		return false, nil
	}, nil)
	if err != nil || errorsSeen != 1 || doneSeen != 1 || finishes != 0 {
		t.Fatalf("failure terminal mismatch: parse=%v errors=%d done=%d success_finishes=%d", err, errorsSeen, doneSeen, finishes)
	}
}

func TestImmediateProgressMetadataDoesNotOpenGate(t *testing.T) {
	for name, metadata := range map[string]string{
		"role":        immediateProgressFrame(map[string]any{"role": "assistant"}, ""),
		"usage":       "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n",
		"finish_only": immediateProgressFrame(map[string]any{}, "stop"),
		"empty_text":  immediateProgressFrame(map[string]any{"reasoning_content": "", "content": ""}, ""),
		"heartbeat":   ": relay-keepalive\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			checked := false
			reader := &immediateProgressReader{steps: []immediateProgressStep{
				{raw: metadata},
				{raw: reasoningGuardTestFrame("fixture-normal-reasoning"), before: func() {
					checked = true
					if rec.Body.Len() != 0 {
						t.Errorf("metadata released %d bytes before real progress", rec.Body.Len())
					}
				}},
				{raw: immediateProgressFrame(map[string]any{}, "stop") + "data: [DONE]\n\n"},
			}}
			if err := Stream(rec, reader, immediateProgressOptions(true, true)); err != nil || !checked {
				t.Fatalf("metadata fixture failed: checkpoint=%t err=%v", checked, err)
			}
		})
	}
}

func TestImmediateProgressFirstPayloadLoopKeepsRetryBoundary(t *testing.T) {
	for _, field := range []string{"reasoning_content", "content"} {
		for _, retry := range []bool{false, true} {
			name := field + "/retry=" + map[bool]string{false: "false", true: "true"}[retry]
			t.Run(name, func(t *testing.T) {
				prefix := immediateProgressFrame(map[string]any{"role": "assistant"}, "") +
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n" +
					immediateProgressFrame(map[string]any{}, "stop")
				loop := immediateProgressFrame(map[string]any{field: strings.Repeat("fixture-loop-marker\n", 400)}, "")
				rec := httptest.NewRecorder()
				err := Stream(rec, strings.NewReader(prefix+loop+"data: [DONE]\n\n"), immediateProgressOptions(true, retry))
				var failure *StreamError
				if !IsLoopGuardError(err) || !errors.As(err, &failure) || failure.Retryable != retry {
					t.Fatalf("first-payload retry mismatch: retry=%t err=%v", retry, err)
				}
				if retry {
					if rec.Body.Len() != 0 {
						t.Fatalf("retryable first loop leaked %d bytes", rec.Body.Len())
					}
				} else {
					if strings.Contains(rec.Body.String(), "fixture-loop-marker") {
						t.Fatal("non-retryable first loop leaked its payload")
					}
					immediateProgressAssertFailure(t, rec.Body.String())
				}
			})
		}
	}
}

func TestImmediateProgressDeliversBeforeNextUpstreamRead(t *testing.T) {
	for _, field := range []string{"reasoning_content", "content"} {
		for _, retry := range []bool{false, true} {
			name := field + "/retry=" + map[bool]string{false: "false", true: "true"}[retry]
			t.Run(name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				checked := false
				reader := &immediateProgressReader{steps: []immediateProgressStep{
					{raw: immediateProgressFrame(map[string]any{field: "ready"}, "")},
					{raw: ": relay-keepalive\n\n", before: func() {
						checked = true
						if !strings.Contains(rec.Body.String(), "ready") {
							t.Error("first real progress was held until a later upstream read")
						}
					}},
					{raw: immediateProgressFrame(map[string]any{}, "stop") + "data: [DONE]\n\n"},
				}}
				if err := Stream(rec, reader, immediateProgressOptions(true, retry)); err != nil || !checked {
					t.Fatalf("early delivery fixture failed: checkpoint=%t err=%v", checked, err)
				}
			})
		}
	}
}

func TestImmediateProgressLoopAfterDeliveryCannotRetry(t *testing.T) {
	firstPayloads := map[string]map[string]any{
		"reasoning": {"reasoning_content": "fixture-progress\n"},
		"content":   {"content": "fixture-progress\n"},
		"tool":      {"tool_calls": []any{map[string]any{"index": 0, "id": "fixture-tool", "type": "function", "function": map[string]any{"name": "read_fixture", "arguments": "{}"}}}},
	}
	for firstName, delta := range firstPayloads {
		for _, loopField := range []string{"reasoning_content", "content"} {
			t.Run(firstName+"/"+loopField, func(t *testing.T) {
				rec := httptest.NewRecorder()
				reader := &immediateProgressReader{steps: []immediateProgressStep{
					{raw: immediateProgressFrame(delta, "")},
					{raw: immediateProgressFrame(map[string]any{loopField: strings.Repeat("fixture-loop-marker\n", 400)}, ""), before: func() {
						if rec.Body.Len() == 0 {
							t.Error("normal progress was not delivered before the later loop")
						}
					}},
					{raw: immediateProgressFrame(map[string]any{}, "stop") + "data: [DONE]\n\n"},
				}}
				err := Stream(rec, reader, immediateProgressOptions(true, true))
				var failure *StreamError
				if !IsLoopGuardError(err) || !errors.As(err, &failure) || failure.Retryable {
					t.Fatalf("visible output lost its non-retry boundary: %v", err)
				}
				if strings.Contains(rec.Body.String(), "fixture-loop-marker") {
					t.Fatal("the frame that triggered the loop was forwarded after detection")
				}
				immediateProgressAssertFailure(t, rec.Body.String())
			})
		}
	}
}

func TestImmediateProgressOptOutPreservesExistingHold(t *testing.T) {
	for _, field := range []string{"reasoning_content", "content"} {
		t.Run(field, func(t *testing.T) {
			rec := httptest.NewRecorder()
			reader := &immediateProgressReader{steps: []immediateProgressStep{
				{raw: immediateProgressFrame(map[string]any{field: "ready\n"}, "")},
				{raw: immediateProgressFrame(map[string]any{field: strings.Repeat("fixture-loop-marker\n", 400)}, ""), before: func() {
					if rec.Body.Len() != 0 {
						t.Errorf("opt-out changed legacy hold-back behavior: %d bytes", rec.Body.Len())
					}
				}},
			}}
			err := Stream(rec, reader, immediateProgressOptions(false, true))
			var failure *StreamError
			if !IsLoopGuardError(err) || !errors.As(err, &failure) || !failure.Retryable || rec.Body.Len() != 0 {
				t.Fatalf("legacy retry changed: bytes=%d err=%v", rec.Body.Len(), err)
			}
		})
	}
}

func TestImmediateProgressPreservesToolDeltas(t *testing.T) {
	tool := func(arguments string, identity bool) map[string]any {
		function := map[string]any{"arguments": arguments}
		call := map[string]any{"index": 0, "function": function}
		if identity {
			call["id"], call["type"], function["name"] = "fixture-tool", "function", "read_fixture"
		}
		return map[string]any{"tool_calls": []any{call}}
	}
	var expected string
	for _, immediate := range []bool{false, true} {
		rec := httptest.NewRecorder()
		reader := &immediateProgressReader{steps: []immediateProgressStep{
			{raw: immediateProgressFrame(tool("{\"path\":", true), "")},
			{raw: immediateProgressFrame(tool("\"fixture\"}", false), ""), before: func() {
				if !strings.Contains(rec.Body.String(), "read_fixture") {
					t.Error("tool identity was withheld before its next argument delta")
				}
			}},
			{raw: immediateProgressFrame(map[string]any{}, "tool_calls") + "data: [DONE]\n\n"},
		}}
		if err := Stream(rec, reader, immediateProgressOptions(immediate, true)); err != nil {
			t.Fatalf("tool deltas failed: immediate=%t err=%v", immediate, err)
		}
		body := rec.Body.String()
		if strings.Count(body, "read_fixture") != 1 || strings.Count(body, "data: [DONE]") != 1 || strings.Contains(body, "\"error\"") {
			t.Fatalf("tool normalization failed: immediate=%t", immediate)
		}
		if !immediate {
			expected = body
		} else if body != expected {
			t.Fatal("immediate progress changed tool arguments or terminal frames")
		}
	}
}

func TestImmediateProgressLoopFailurePreservesObservedUsage(t *testing.T) {
	loopFrame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"reasoning_content": strings.Repeat("fixture-loop-marker\n", 400),
		}}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 20,
			"total_tokens": 120, "prompt_tokens_details": map[string]any{"cached_tokens": 80}},
	})
	rec := httptest.NewRecorder()
	raw := reasoningGuardTestFrame("fixture-normal-reasoning\n") + "data: " + string(loopFrame) + "\n\n"
	err := Stream(rec, strings.NewReader(raw), immediateProgressOptions(true, true))
	if !IsLoopGuardError(err) {
		t.Fatalf("usage fixture did not detect its loop: %v", err)
	}
	var seen map[string]any
	if err := readSSE(strings.NewReader(rec.Body.String()), func(event sseEvent) (bool, error) {
		if event.data == "[DONE]" {
			return false, nil
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(event.data), &frame); err != nil {
			return false, err
		}
		if usage, ok := frame["usage"].(map[string]any); ok {
			seen = usage
		}
		return false, nil
	}, nil); err != nil {
		t.Fatalf("usage fixture emitted invalid SSE: %v", err)
	}
	if seen == nil || seen["prompt_tokens"] != float64(100) || seen["completion_tokens"] != float64(20) || seen["total_tokens"] != float64(120) {
		t.Fatal("discarding a loop payload also discarded its observed usage")
	}
	details, _ := seen["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(80) {
		t.Fatal("loop failure lost the observed cache usage")
	}
	if strings.Contains(rec.Body.String(), "fixture-loop-marker") {
		t.Fatal("preserving usage leaked the loop payload")
	}
	immediateProgressAssertFailure(t, rec.Body.String())
}
