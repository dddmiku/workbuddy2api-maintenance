// ═══ 更新日志 ═══
// 2026-09-24：新增「上下文裁剪必须回真体积」与「超限幅度上限」的回归：
//
//	小幅超限照旧裁剪、大幅超限交回客户端、裁剪后上报的 input_tokens
//	必须是上游给出的原始体积。
package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestPromptTooLongCountsParsesUpstreamBody 解析上游 11115 原文里的两个数字。
func TestPromptTooLongCountsParsesUpstreamBody(t *testing.T) {
	cases := []struct {
		body    string
		tokens  int
		maximum int
		ok      bool
	}{
		{`{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum"}`, 1048691, 1048576, true},
		{`{"code":11115,"msg":"prompt is too long: 2015759 tokens > 1048576 maximum"}`, 2015759, 1048576, true},
		{`{"code":11115,"msg":"prompt is too long: 1048576 > 1048576"}`, 1048576, 1048576, true},
		{`{"code":11115,"msg":"prompt is too long"}`, 0, 0, false},
		{`{"code":11115,"msg":""}`, 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		tokens, maximum, ok := PromptTooLongCounts(tc.body)
		if ok != tc.ok || tokens != tc.tokens || maximum != tc.maximum {
			t.Errorf("PromptTooLongCounts(%q)=(%d,%d,%v) want (%d,%d,%v)",
				tc.body, tokens, maximum, ok, tc.tokens, tc.maximum, tc.ok)
		}
	}
}

// TestWithinTrimOvershootLimit 只对「差一点点」裁剪；大幅超限必须交回客户端。
func TestWithinTrimOvershootLimit(t *testing.T) {
	cases := []struct {
		name    string
		tokens  int
		maximum int
		want    bool
	}{
		{"just_over", 1048691, 1048576, true},
		{"under", 1048000, 1048576, true},
		{"exactly_at_limit", 1048576, 1048576, true},
		{"ten_percent_over", 1153000, 1048576, true},
		{"double", 2015759, 1048576, false},
		{"far_over", 3386426, 1048576, false},
		{"missing_tokens", 0, 1048576, false},
		{"missing_maximum", 1048691, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithinTrimOvershootLimit(tc.tokens, tc.maximum); got != tc.want {
				t.Errorf("WithinTrimOvershootLimit(%d,%d)=%v want %v",
					tc.tokens, tc.maximum, got, tc.want)
			}
		})
	}
}

// TestOverrideUsagePromptSize 回真只改输入与合计，保留输出与缓存字段。
func TestOverrideUsagePromptSize(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           float64(74332),
		"completion_tokens":       float64(550),
		"total_tokens":            float64(74882),
		"prompt_cache_hit_tokens": float64(36224),
	}
	out := overrideUsagePromptSize(usage, 2015759)
	if got := intOfAny(out["prompt_tokens"]); got != 2015759 {
		t.Errorf("prompt_tokens=%d want 2015759", got)
	}
	if got := intOfAny(out["completion_tokens"]); got != 550 {
		t.Errorf("completion_tokens must stay upstream: %d", got)
	}
	if got := intOfAny(out["total_tokens"]); got != 2016309 {
		t.Errorf("total_tokens=%d want 2016309", got)
	}
	if got := intOfAny(out["prompt_cache_hit_tokens"]); got != 36224 {
		t.Errorf("cached tokens must stay upstream: %d", got)
	}
	// 原 map 不被改写（调用方可能仍持有上游原值）。
	if got := intOfAny(usage["prompt_tokens"]); got != 74332 {
		t.Errorf("input map was mutated: %d", got)
	}
	// 上游已报更大值时不得缩小。
	if out := overrideUsagePromptSize(usage, 1000); intOfAny(out["prompt_tokens"]) != 74332 {
		t.Error("override must not shrink a larger upstream value")
	}
}

// TestContextTrimRecorderKeepsFirstOriginal 多档裁剪只记首个（最大的）原始体积。
func TestContextTrimRecorderKeepsFirstOriginal(t *testing.T) {
	ctx, info := WithContextTrimRecorder(context.Background())
	recordContextTrim(ctx, 2015759, 1048576)
	recordContextTrim(ctx, 1732130, 1048576) // 第二档：已裁过的中间值
	recordContextTrim(ctx, 1055774, 1048576)
	if info.OriginalPromptTokens != 2015759 {
		t.Fatalf("original=%d want 2015759 (first, largest)", info.OriginalPromptTokens)
	}
	if !info.Trimmed {
		t.Fatal("Trimmed should be set")
	}
	if got := intOfAny(info.override(map[string]any{"prompt_tokens": float64(74332)})["prompt_tokens"]); got != 2015759 {
		t.Fatal("override should report the original size")
	}
}

// TestTrimInfoNilIsNoop 未挂观测槽时零改动（既有调用方行为不变）。
func TestTrimInfoNilIsNoop(t *testing.T) {
	var info *ContextTrimInfo
	usage := map[string]any{"prompt_tokens": float64(10)}
	if got := info.override(usage); got["prompt_tokens"] != float64(10) {
		t.Fatal("nil TrimInfo must be a no-op")
	}
	if streamTrimInfo(nil) != nil {
		t.Fatal("no options must yield nil TrimInfo")
	}
}

// TestLargeOvershootIsReturnedToClient 大幅超限时不再裁剪：11115 原样交回客户端，
// 让 Codex 走自己的压缩流程（这正是修复前缺失的信号）。
// TestContextTooLongWithoutCountsIsNotSilentlyTrimmed 解析不到体积时不静默裁剪：
// 没有权威数字就无法判断超限幅度、也无法把原始体积回真给客户端，此时把上游错误
// 原样交回，比「猜着裁」安全。
func TestContextTooLongWithoutCountsIsNotSilentlyTrimmed(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11115,"msg":"prompt is too long"}`))
	}))
	defer srv.Close()

	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	_, status, raw, err := client.ChatStreamContext(
		context.Background(), &auth.Auth{UID: "account-nocount"}, buildTurns(t, 12), "", ChatMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", status)
	}
	if !strings.Contains(string(raw), "prompt is too long") {
		t.Fatalf("upstream error must be passed through: %s", raw)
	}
	if attempts != 1 {
		t.Fatalf("unparseable size must not be trimmed: attempts=%d", attempts)
	}
}

func TestLargeOvershootIsReturnedToClient(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11115,"msg":"prompt is too long: 2015759 tokens > 1048576 maximum",` +
			`"extError":{"code":"context_length_exceeded"}}`))
	}))
	defer srv.Close()

	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	body := buildTurns(t, 40)
	_, status, raw, err := client.ChatStreamContext(
		context.Background(), &auth.Auth{UID: "account-big"}, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", status)
	}
	if !strings.Contains(string(raw), "prompt is too long") {
		t.Fatalf("upstream error must be passed through: %s", raw)
	}
	if attempts != 1 {
		t.Fatalf("large overshoot must not be trimmed: attempts=%d", attempts)
	}
}

// TestSmallOvershootIsStillTrimmed 小幅超限照旧自动裁剪并成功。
func TestSmallOvershootIsStillTrimmed(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":11115,"msg":"prompt is too long: 1048691 tokens > 1048576 maximum"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	body := buildTurns(t, 8)
	rc, status, raw, err := client.ChatStreamContext(
		context.Background(), &auth.Auth{UID: "account-small"}, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if attempts != 2 {
		t.Fatalf("small overshoot should retry once: attempts=%d", attempts)
	}
}

// TestTrimmedStreamReportsOriginalPromptSize 裁剪后的流式响应必须把**原始**体积
// 回真给客户端——否则客户端以为上下文很小，压缩机制永不触发。
func TestTrimmedStreamReportsOriginalPromptSize(t *testing.T) {
	info := &ContextTrimInfo{}
	recordContextTrim(context.Background(), 0, 0) // 无 ctx：不应 panic
	ctx, info2 := WithContextTrimRecorder(context.Background())
	recordContextTrim(ctx, 2015759, 1048576)
	info = info2

	raw := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":74332,\"completion_tokens\":550,\"total_tokens\":74882}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw), StreamOptions{TrimInfo: info}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	var seen map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if usage, ok := frame["usage"].(map[string]any); ok {
			seen = usage
		}
	}
	if seen == nil {
		t.Fatalf("no usage frame in output: %s", body)
	}
	if got := intOfAny(seen["prompt_tokens"]); got != 2015759 {
		t.Fatalf("client saw prompt_tokens=%d, want the original 2015759", got)
	}
}

// intOfAny 读一个 JSON 数字（float64 或 int）为 int。
func intOfAny(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return -1
}
