// ═══ 更新日志 ═══
// 2026-09-26：请求决定的 400 终态保留失败方自己的会话绑定，仍验证不影响其他调用方。
// 2026-09-19：通过真实鉴权及 Chat/Responses Handler 锁定跨调用密钥会话隔离、失败解绑和关联头隔离。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

func callerSessionFixture(t *testing.T, secondFails bool) (*Handler, []string, *[]http.Header, *bindStore) {
	t.Helper()
	h, _ := postreleaseUsageHandler(t, sseOK)
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "caller-pool-a", AccessToken: "synthetic-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "caller-pool-b", AccessToken: "synthetic-b", ExpiresAt: 9999999999},
	)
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "synthetic-keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.APIKeys = store
	var keys []string
	for _, label := range []string{"fixture-a", "fixture-b"} {
		_, key, err := store.Create(label, "session isolation regression", nil)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings, Available: h.cfg.Pool.AvailableUIDs})
	var headers []http.Header
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		headers = append(headers, req.Header.Clone())
		if secondFails && len(headers) == 2 {
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11101,"msg":"Unmarshal chat params failed"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})
	return h, keys, &headers, bindings
}

func callerSessionRequest(t *testing.T, path, scenario string, actor int, key string) *http.Request {
	t.Helper()
	body := map[string]any{"model": "cn:hy3", "stream": true}
	if path == "/v1/responses" {
		body["input"] = "continue"
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "continue"}}
	}
	switch scenario {
	case "conversation", "headers":
		body["metadata"] = map[string]any{"conversation_id": "shared-session"}
	case "client-thread":
		body["client_metadata"] = map[string]any{"thread_id": "shared-thread"}
	case "explicit-over-cache":
		body["prompt_cache_key"] = "shared-cache"
		body["conversation_id"] = fmt.Sprintf("conversation-%d", actor)
	case "cache", "sticky-off":
		body["prompt_cache_key"] = "shared-cache"
	case "user":
		body["metadata"] = map[string]any{"user_id": "generic-client-user"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+key)
	if scenario == "headers" {
		req.Header.Set("X-Conversation-Request-ID", "0123456789abcdef0123456789abcdef")
		req.Header.Set("X-Trace-ID", "fedcba9876543210fedcba9876543210")
	}
	return req
}

func TestCallerSessionIsolation(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, scenario := range []string{"conversation", "client-thread", "explicit-over-cache", "cache", "user", "headers", "no-session", "sticky-off"} {
			t.Run(path+"/"+scenario, func(t *testing.T) {
				h, keys, captured, bindings := callerSessionFixture(t, false)
				if scenario == "sticky-off" {
					h.cfg.Session = nil
				}
				for _, actor := range []int{0, 1, 0} {
					rr := httptest.NewRecorder()
					h.ServeHTTP(rr, callerSessionRequest(t, path, scenario, actor, keys[actor]))
					if rr.Code != 200 {
						t.Fatalf("request status=%d", rr.Code)
					}
				}
				if len(*captured) != 3 {
					t.Fatalf("upstream calls=%d, want 3", len(*captured))
				}
				a, b, a2 := (*captured)[0], (*captured)[1], (*captured)[2]
				for _, header := range []string{"X-Conversation-Request-ID", "X-Conversation-ID", "X-Trace-ID"} {
					if a.Get(header) != "" && a.Get(header) == b.Get(header) {
						t.Errorf("different authenticated callers shared %s", header)
					}
					if a.Get(header) != a2.Get(header) {
						t.Errorf("same caller changed %s", header)
					}
				}
				if a.Get("X-Request-ID") == b.Get("X-Request-ID") {
					t.Error("request IDs must be unique")
				}
				wantBindings := 2
				// no-session：客户端一个会话标识都不发（narrafork 形态），网关按正文派生
				// 对话级回退键补上粘性，因此仍然建立绑定（每个调用方各一份，互不串号）。
				if scenario == "sticky-off" {
					wantBindings = 0
				}
				if len(bindings.LoadBinds()) != wantBindings {
					t.Errorf("bindings=%d want=%d", len(bindings.LoadBinds()), wantBindings)
				}
				if wantBindings == 2 {
					if a.Get("X-User-Id") == b.Get("X-User-Id") {
						t.Error("different callers shared one binding despite an idle account")
					}
					if a.Get("X-User-Id") != a2.Get("X-User-Id") {
						t.Error("caller A lost its affinity")
					}
				}
				if snapshot := h.cfg.Usage.Snapshot(); len(snapshot.Keys) != 2 || snapshot.Totals.Requests != 3 {
					t.Fatal("usage must remain independently accounted")
				}
			})
		}
	}
}

func TestCallerFailureDoesNotUnbindAnotherCaller(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h, keys, captured, bindings := callerSessionFixture(t, true)
			first := httptest.NewRecorder()
			h.ServeHTTP(first, callerSessionRequest(t, path, "conversation", 0, keys[0]))
			before := bindings.LoadBinds()
			second := httptest.NewRecorder()
			h.ServeHTTP(second, callerSessionRequest(t, path, "conversation", 1, keys[1]))
			after := bindings.LoadBinds()
			// B 的失败由请求本身决定（上游 400 参数错误）：B 保留自己的会话绑定以维持同号缓存，
			// 但绝不能改动 A 的绑定。
			if first.Code != 200 || second.Code != 400 || len(before) != 1 || len(after) != 2 {
				t.Fatalf("statuses=%d/%d bindings=%d/%d; caller B must not unbind A", first.Code, second.Code, len(before), len(after))
			}
			for key, uid := range before {
				if after[key] != uid {
					t.Error("caller A's binding was changed by B's failure")
				}
			}
			third := httptest.NewRecorder()
			h.ServeHTTP(third, callerSessionRequest(t, path, "conversation", 0, keys[0]))
			if third.Code != 200 || (*captured)[0].Get("X-User-Id") != (*captured)[2].Get("X-User-Id") {
				t.Error("caller A did not retain its account")
			}
		})
	}
}

func TestExplicitConversationsOverrideSharedCacheForSameCaller(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h, keys, captured, bindings := callerSessionFixture(t, false)
			for _, actor := range []int{0, 1, 0} {
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, callerSessionRequest(t, path, "explicit-over-cache", actor, keys[0]))
				if rr.Code != 200 {
					t.Fatalf("status=%d", rr.Code)
				}
			}
			if len(bindings.LoadBinds()) != 2 || (*captured)[0].Get("X-User-Id") == (*captured)[1].Get("X-User-Id") {
				t.Error("shared cache key overrode two explicit conversations")
			}
		})
	}
}

// narraforkFixture 构造专供 narrafork 形态回归的夹具：账号属 global 域，与线上
// 实测一致（narrafork 走的是 global:deepseek-v4.1-flash）。
func narraforkFixture(t *testing.T) (*Handler, string, *[]http.Header, *bindStore) {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	h, _ := postreleaseUsageHandler(t, sseOK)
	h.cfg.Pool = testPoolWith(
		&auth.Auth{UID: "narrafork-a", Domain: "www.workbuddy.ai", AccessToken: "synthetic-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "narrafork-b", Domain: "www.workbuddy.ai", AccessToken: "synthetic-b", ExpiresAt: 9999999999},
	)
	store, err := apikeys.Open(filepath.Join(t.TempDir(), "narrafork-keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.APIKeys = store
	_, key, err := store.Create("narrafork", "session-less client regression", nil)
	if err != nil {
		t.Fatal(err)
	}
	bindings := newBindStore()
	h.cfg.Session = session.New(session.Config{TTL: time.Hour, Store: bindings, Available: h.cfg.Pool.AvailableUIDs})
	var headers []http.Header
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		headers = append(headers, req.Header.Clone())
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})
	return h, key, &headers, bindings
}

// narraforkRequest 复刻线上实测的 narrafork /v1/chat/completions 形状：顶层只有
// model / messages / stream / stream_options / max_tokens / tools / tool_choice /
// reasoning_effort，**没有任何**会话标识字段（无 conversation_id、无 metadata、
// 无 prompt_cache_key）。抓包证据见本轮排查记录。
func narraforkRequest(t *testing.T, key, firstUser, lastUser string) *http.Request {
	t.Helper()
	messages := []any{
		map[string]any{"role": "system", "content": "You are Cascade, a powerful agentic AI coding assistant."},
		map[string]any{"role": "user", "content": firstUser},
	}
	if lastUser != "" {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": "好的"},
			map[string]any{"role": "user", "content": lastUser})
	}
	raw, err := json.Marshal(map[string]any{
		"model": "global:deepseek-v4.1-flash", "messages": messages, "stream": true,
		"stream_options": map[string]any{"include_usage": true}, "max_tokens": 8192,
		"tools": []any{}, "tool_choice": "auto", "reasoning_effort": "max",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "narrafork/0.7.5 (Windows 10.0.19045; x64) Bun/1.4.2")
	return req
}

// TestSessionLessClientKeepsAccountAffinity 回归：一个会话标识都不发的客户端
// （narrafork 形态）在同一对话内必须保持同一个账号，不能每轮换号。
//
// 修复前的实际表现：ExtractKey 恒空 → 粘性完全不生效 → 每轮 Pick 随机换号 →
// 上游提示缓存整段失效（线上日志里这类请求 hit 恒为 0，而带 prompt_cache_key 的
// Codex 那路 hit 稳定在数十万）。
func TestSessionLessClientKeepsAccountAffinity(t *testing.T) {
	h, key, captured, bindings := narraforkFixture(t)

	// 同一对话推进三轮：首条 user 不变，末条 user 每轮不同。
	turns := []struct{ first, last string }{
		{"分析这个 APK 的签名校验", ""},
		{"分析这个 APK 的签名校验", "继续"},
		{"分析这个 APK 的签名校验", "再看下一处"},
	}
	for _, turn := range turns {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, narraforkRequest(t, key, turn.first, turn.last))
		if rr.Code != 200 {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	if len(*captured) != 3 {
		t.Fatalf("upstream calls=%d want 3", len(*captured))
	}
	first := (*captured)[0].Get("X-User-Id")
	for i, header := range *captured {
		if header.Get("X-User-Id") != first {
			t.Fatalf("turn %d switched accounts: %s vs %s (同一对话必须保持粘性)", i, header.Get("X-User-Id"), first)
		}
	}
	if len(bindings.LoadBinds()) != 1 {
		t.Errorf("bindings=%d want 1（同一对话只应有一份绑定）", len(bindings.LoadBinds()))
	}

	// 另一条对话（首条 user 不同）应绑到另一份键上：粘性是按对话隔离的。
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, narraforkRequest(t, key, "完全不同的另一个任务", ""))
	if rr.Code != 200 {
		t.Fatalf("status=%d", rr.Code)
	}
	if len(bindings.LoadBinds()) != 2 {
		t.Errorf("bindings=%d want 2（不同对话应各自绑定）", len(bindings.LoadBinds()))
	}
}

// TestSessionLessClientRespectsStickyOff 关掉粘性（Session==nil）时，回退键也不应生效。
func TestSessionLessClientRespectsStickyOff(t *testing.T) {
	h, key, _, bindings := narraforkFixture(t)
	h.cfg.Session = nil
	for _, last := range []string{"", "继续"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, narraforkRequest(t, key, "分析这个 APK 的签名校验", last))
		if rr.Code != 200 {
			t.Fatalf("status=%d", rr.Code)
		}
	}
	if len(bindings.LoadBinds()) != 0 {
		t.Errorf("bindings=%d want 0（粘性关闭时不应建立绑定）", len(bindings.LoadBinds()))
	}
}
