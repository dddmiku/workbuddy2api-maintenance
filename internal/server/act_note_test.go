// ═══ 更新日志 ═══
// 2026-09-20：补原生 /v1/chat/completions 的运行约定回归：Devin 等只走 Chat 的客户端
//
//	此前拿不到这条约定（applyActNote 只挂在 Responses 路径上），模型回一句
//	「让我先确认」就结束本轮，用户必须手动发「继续」。
//
// 2026-09-17：锁定运行约定的注入位置与开关：只在带工具的请求上追加到 system 末尾，
//
//	原 system 内容不改，关闭时不注入。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// outbound 保存最近一次到达假上游的请求体（供本文件用例断言）。
var outbound []byte

// firstSystem 取首条 system 消息的文本（decodeChat 复用 responses_test.go 里的同名助手）。
func firstSystem(t *testing.T, obj map[string]any) string {
	t.Helper()
	msgs, _ := obj["messages"].([]any)
	for _, item := range msgs {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			content, _ := msg["content"].(string)
			return content
		}
	}
	return ""
}

func TestApplyActNoteAppendsToSystemWhenToolsPresent(t *testing.T) {
	body := []byte(`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"system","content":"You are Codex."},{"role":"user","content":"hi"}]}`)
	out := applyActNote(body, prompt.ActNote, true)
	obj := decodeChat(t, out)
	system := firstSystem(t, obj)
	if !strings.HasPrefix(system, "You are Codex.") {
		t.Fatalf("原 system 被改写: %q", system)
	}
	if !strings.Contains(system, prompt.ActNote) {
		t.Fatalf("运行约定未追加: %q", system)
	}
	// 用户消息与其余字段一字不动。
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2", len(msgs))
	}
	if user := msgs[1].(map[string]any); user["content"] != "hi" {
		t.Fatalf("用户消息被改动: %v", user["content"])
	}
}

func TestApplyActNoteSkippedWithoutTools(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex."}]}`)
	out := applyActNote(body, prompt.ActNote, false)
	if strings.Contains(string(out), "act instead of narrating") {
		t.Fatalf("无工具请求不应注入运行约定: %s", out)
	}
}

func TestApplyActNoteCreatesSystemWhenMissing(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	out := applyActNote(body, prompt.ActNote, true)
	obj := decodeChat(t, out)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2", len(msgs))
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("首条应为 system，实际 %q", role)
	}
}

func TestApplyActNoteEmptyNoteIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex."}]}`)
	out := applyActNote(body, "", true)
	if string(out) != string(body) {
		t.Fatalf("note 为空时不应改动 body")
	}
}

func TestActNoteForResolvesConfig(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", prompt.ActNote},
		{"off", ""},
		{"none", ""},
		{"false", ""},
		{"  ", prompt.ActNote},
		{"自定义约定", "自定义约定"},
	}
	for _, c := range cases {
		if got := ActNoteFor(c.configured); got != c.want {
			t.Errorf("ActNoteFor(%q)=%q want %q", c.configured, got, c.want)
		}
	}
}

func TestChatBodyHasToolsDetectsDeclarations(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"modern_tools", `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`, true},
		{"legacy_functions", `{"model":"m","messages":[],"functions":[{"name":"f"}]}`, true},
		{"empty_tools", `{"model":"m","messages":[],"tools":[]}`, false},
		{"no_tools", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"malformed", `not json`, false},
	}
	for _, c := range cases {
		if got := chatBodyHasTools([]byte(c.body)); got != c.want {
			t.Errorf("%s: chatBodyHasTools=%v want %v", c.name, got, c.want)
		}
	}
}

// TestActNoteReachesNativeChatPath 复现 Devin 的调用形态：只走 /v1/chat/completions、
// 带工具。修复前 applyActNote 只挂在 Responses 路径上，这类客户端永远拿不到运行约定，
// 模型回一句进度叙述就结束本轮，用户只能手动发「继续」。
func TestActNoteReachesNativeChatPath(t *testing.T) {
	var captured []byte
	h, _ := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+"data: [DONE]\n\n")
	// 生产由 main.go 经 ActNoteFor 解析后注入；测试里直接构造 Handler，需显式设置。
	h.cfg.PromptActNote = ActNoteFor("")
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		captured = raw
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(postreleaseUsageContent + postreleaseFinish("stop") + "data: [DONE]\n\n")),
		}, nil
	})
	body := `{"model":"cn:deepseek-v4.1-flash","stream":true,` +
		`"messages":[{"role":"system","content":"You are Codex."},{"role":"user","content":"do the task"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(captured) == 0 {
		t.Fatal("upstream never received the request")
	}
	var obj map[string]any
	if err := json.Unmarshal(captured, &obj); err != nil {
		t.Fatalf("captured body is not JSON: %v", err)
	}
	system := firstSystem(t, obj)
	if !strings.HasPrefix(system, "You are Codex.") {
		t.Fatalf("原 system 被改写: %q", system)
	}
	if !strings.Contains(system, prompt.ActNote) {
		t.Fatalf("原生 Chat 路径未注入运行约定: %q", system)
	}
	// 工具声明与用户消息必须原样保留。
	if tools, _ := obj["tools"].([]any); len(tools) != 1 {
		t.Fatalf("工具声明被改动: %v", obj["tools"])
	}
}

// TestActNoteSkippedOnNativeChatWithoutTools 纯对话（无工具）不应被注入约定。
func TestActNoteSkippedOnNativeChatWithoutTools(t *testing.T) {
	var captured []byte
	h, _ := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+"data: [DONE]\n\n")
	h.cfg.PromptActNote = ActNoteFor("")
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		captured = raw
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(postreleaseUsageContent + postreleaseFinish("stop") + "data: [DONE]\n\n")),
		}, nil
	})
	body := `{"model":"cn:deepseek-v4.1-flash","stream":true,` +
		`"messages":[{"role":"system","content":"You are Codex."},{"role":"user","content":"just chat"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(string(captured), prompt.ActNote) {
		t.Fatal("无工具请求不应注入运行约定")
	}
}

// TestActNoteNotInjectedTwiceOnResponsesPath 锁定两条路径的边界：/v1/responses 在
// responsesToChat 之后注入一次，随后把请求交给 chatCompletions；后者必须靠 writer
// 类型判断跳过，否则同一条约定会被追加两遍。
func TestActNoteNotInjectedTwiceOnResponsesPath(t *testing.T) {
	var captured []byte
	h, _ := postreleaseUsageHandler(t, postreleaseUsageContent+postreleaseFinish("stop")+"data: [DONE]\n\n")
	h.cfg.PromptActNote = ActNoteFor("")
	h.cfg.Upstream.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		captured = raw
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(postreleaseUsageContent + postreleaseFinish("stop") + "data: [DONE]\n\n")),
		}, nil
	})
	body := `{"model":"cn:deepseek-v4.1-flash","stream":true,"instructions":"You are Codex.",` +
		`"input":"do the task",` +
		`"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if len(captured) == 0 {
		t.Fatal("upstream never received the request")
	}
	if got := strings.Count(string(captured), prompt.ActNote); got != 1 {
		t.Fatalf("运行约定被注入 %d 次，want 1", got)
	}
}

// TestActNoteSurvivesCustomPromptMode custom 模式下 prompt.Rewrite 会删掉所有
// system/developer 消息再插入配置提示词，而运行约定正是追加到 system 里。
// 注入若排在 Rewrite 之前，约定会被连同旧 system 一起删掉，永远到不了模型，
// 而文档承诺它一定生效。
//
// 2026-09-30 深度体检发现。
func TestActNoteSurvivesCustomPromptMode(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			outbound = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
						"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
						"data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		APIKey:        "k",
		Pool:          testPoolWith(&auth.Auth{UID: "u1", AccessToken: "t", ExpiresAt: 9999999999}),
		Upstream:      up,
		PromptMode:    "custom",
		PromptText:    "You are the gateway prompt.",
		PromptActNote: prompt.ActNote,
	})
	body := `{"model":"cn:fixture","stream":true,"max_tokens":64,"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{}}}}],"messages":[{"role":"system","content":"client system"},{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(w, r)
	if outbound == nil {
		t.Fatal("请求未到达上游")
	}
	obj := decodeChat(t, outbound)
	system := firstSystem(t, obj)
	if !strings.Contains(system, "You are the gateway prompt.") {
		t.Fatalf("custom 提示词未生效: %q", system)
	}
	if !strings.Contains(system, prompt.ActNote) {
		t.Fatalf("运行约定在 custom 模式下丢失（被 prompt.Rewrite 删掉）: %q", system)
	}
	if strings.Contains(system, "client system") {
		t.Fatalf("custom 模式应替换客户端 system: %q", system)
	}
}
