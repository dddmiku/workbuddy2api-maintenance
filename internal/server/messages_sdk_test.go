// ═══ 更新日志 ═══
// 2026-09-25：固定官方 Anthropic SDK 对实际 Handler 与回环假上游做可选集成测试，验证最终计数、工具回调与失败边界。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func TestMessagesOfficialSDKContract(t *testing.T) {
	script := os.Getenv("WB2API_ANTHROPIC_SDK_CONTRACT")
	if script == "" {
		t.Skip("set WB2API_ANTHROPIC_SDK_CONTRACT to the pinned offline SDK handler-contract.mjs")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the explicitly requested SDK contract test")
	}
	permit := make(chan struct{})
	var release sync.Once
	releaseTools := func() { release.Do(func() { close(permit) }) }
	defer releaseTools()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		kind := "text"
		for _, name := range []string{"text", "faithful_usage", "thinking", "parallel_tools", "invalid_tools", "unknown_usage", "nonstream"} {
			if strings.Contains(string(body), "sdk-case:"+name+"\"") {
				kind = name
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(delta map[string]any, finish any, usage map[string]any) {
			chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			if usage != nil {
				chunk["usage"] = usage
			}
			encoded, _ := json.Marshal(chunk)
			_, _ = io.WriteString(w, "data: "+string(encoded)+"\n\n")
			w.(http.Flusher).Flush()
		}
		usage := map[string]any{"prompt_tokens": 160, "prompt_cache_hit_tokens": 60, "completion_tokens": 7, "completion_thinking_tokens": 3}
		finish := "stop"
		switch kind {
		case "parallel_tools", "invalid_tools":
			second := `{"x":2}`
			if kind == "invalid_tools" {
				second = `{"x":`
			}
			emit(map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "one", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"x":1}`}},
				map[string]any{"index": 1, "id": "two", "type": "function", "function": map[string]any{"name": "read", "arguments": second}},
			}}, nil, nil)
			finish = "tool_calls"
			if kind == "parallel_tools" {
				select {
				case <-permit:
				case <-r.Context().Done():
					return
				}
			}
		case "thinking":
			emit(map[string]any{"reasoning_content": "consider"}, nil, nil)
			emit(map[string]any{"content": "answer"}, nil, nil)
		default:
			emit(map[string]any{"content": "hello"}, nil, nil)
		}
		if kind == "faithful_usage" {
			usage["prompt_tokens"] = 100
		}
		if kind == "unknown_usage" {
			usage = nil
		}
		emit(map[string]any{}, finish, usage)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer remote.Close()
	h := NewHandler(Config{APIKey: "sk-ant-fixture-no-real-credential", Pool: testPoolWith(&auth.Auth{UID: "sdk-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: &upstream.Client{HTTP: remote.Client(), ChatHTTP: remote.Client(), ChatBaseCN: remote.URL}})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__fixture/release-tools" {
			releaseTools()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// #nosec G204 -- This opt-in test invokes a locally supplied contract script,
	// with a newly created loopback endpoint and fake credentials only.
	command := exec.CommandContext(ctx, node, script, gateway.URL)
	output, err := command.CombinedOutput()
	releaseTools()
	t.Logf("official SDK contract: %s", output)
	if err != nil {
		t.Fatalf("official SDK rejected the actual handler contract: %v", err)
	}
}
