// ═══ 更新日志 ═══
// 2026-09-28：messages 里 role=system/developer 折进系统提示、role=tool 按 user 处理；只有 system 的请求仍拒。
// 2026-09-26：顶层 document 块改为占位（不再算无效输入），无效输入清单同步。
// 2026-09-25：通过真实HTTP入口验证messages工具、流事件、缓存口径、鉴权、压缩和失败终态。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

func messagesFixture(t *testing.T, reply string) (*Handler, *map[string]any, *int, *string) {
	t.Helper()
	var outbound map[string]any
	calls := 0
	trace := ""
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		trace = r.Header.Get("X-Gateway-Request-ID")
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(remote.Close)
	h := NewHandler(Config{APIKey: "fixture-key", Pool: testPoolWith(&auth.Auth{UID: "messages-fixture", AccessToken: "fixture", ExpiresAt: 9999999999}), Upstream: &upstream.Client{HTTP: remote.Client(), ChatHTTP: remote.Client(), ChatBaseCN: remote.URL}})
	return h, &outbound, &calls, &trace
}

func TestMessagesTextAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			h, out, calls, trace := messagesFixture(t, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":60}}\n\ndata: [DONE]\n\n")
			body, _ := json.Marshal(map[string]any{"model": "cn:fixture", "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(encodedRequest(t, "gzip", body)))
			r.Header.Set("X-API-Key", "fixture-key")
			r.Header.Set("Content-Encoding", "gzip")
			r.Header.Set("X-Request-ID", "forged")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || *calls != 1 {
				t.Fatalf("%d %d %s", w.Code, *calls, w.Body.String())
			}
			if (*out)["model"] != "fixture" || *trace == "" || *trace != w.Header().Get("X-Request-ID") || *trace == "forged" {
				t.Fatalf("routing/trace mismatch: %v %s", (*out)["model"], *trace)
			}
			if !strings.Contains(w.Body.String(), `"input_tokens":40`) || !strings.Contains(w.Body.String(), `"cache_read_input_tokens":60`) || !strings.Contains(w.Body.String(), `"output_tokens":7`) {
				t.Fatalf("usage must split cached input without double counting: %s", w.Body.String())
			}
			if stream {
				if strings.Count(w.Body.String(), "event: message_stop") != 1 || !strings.Contains(w.Body.String(), "event: content_block_delta") {
					t.Fatalf("invalid event lifecycle: %s", w.Body.String())
				}
			} else {
				var message map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &message); err != nil || message["type"] != "message" {
					t.Fatalf("invalid message: %s", w.Body.String())
				}
			}
		})
	}
}

func TestMessagesParallelToolsAndImageResults(t *testing.T) {
	input := `{"model":"cn:fixture","max_tokens":100,"system":[{"type":"text","text":"keep instructions"}],"messages":[{"role":"user","content":"run tools"},{"role":"assistant","content":[{"type":"thinking","thinking":"history"},{"type":"tool_use","id":"one","name":"read","input":{"x":1}},{"type":"tool_use","id":"two","name":"read","input":{"x":2}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"two","is_error":true,"content":"failed"},{"type":"tool_result","tool_use_id":"one","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]},{"type":"text","text":"compare"}]}],"tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"read","disable_parallel_tool_use":true}}`
	chat, _, _, err := messagesToChat([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateChatRequest(chat); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	_ = json.Unmarshal(chat, &object)
	messages := object["messages"].([]any)
	if len(messages) != 6 || messages[2].(map[string]any)["reasoning_content"] != "history" || messages[3].(map[string]any)["role"] != "tool" || messages[4].(map[string]any)["role"] != "tool" {
		t.Fatalf("history or pairing lost: %s", chat)
	}
	if !strings.Contains(string(chat), "data:image/png;base64,YWJj") || !strings.Contains(string(chat), "tool execution error") || object["parallel_tool_calls"] != false {
		t.Fatalf("media/tool controls lost: %s", chat)
	}
}

func TestMessagesInvalidInputsNeverReachUpstream(t *testing.T) {
	for _, body := range []string{
		`{"model":"cn:fixture","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"cn:fixture","max_tokens":5,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"orphan","content":"x"}]}]}`,
		`{"model":"cn:fixture","max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"missing","name":"read","input":{}}]}]}`,
	} {
		h, _, calls, _ := messagesFixture(t, sseOK)
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		r.Header.Set("X-API-Key", "fixture-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || *calls != 0 || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
			t.Fatalf("invalid request accepted: %d %d %s", w.Code, *calls, w.Body.String())
		}
	}
}

func TestMessagesAuthAndUnsupportedCount(t *testing.T) {
	h, _, calls, _ := messagesFixture(t, sseOK)
	for _, key := range []string{"wrong", "fixture-key"} {
		r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader("{}"))
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := 401
		if key == "fixture-key" {
			want = 501
		}
		if w.Code != want || *calls != 0 || strings.Contains(w.Body.String(), `"input_tokens"`) {
			t.Fatalf("unexpected count result: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestMessagesToolStreamValidatedBeforeStop(t *testing.T) {
	for _, valid := range []bool{false, true} {
		args := `{"x":`
		if valid {
			args = `{"x":1}`
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": args}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4}})
		h, _, _, _ := messagesFixture(t, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"cn:fixture","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"read"}],"tools":[{"name":"read","input_schema":{"type":"object"}}]}`))
		r.Header.Set("X-API-Key", "fixture-key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if valid && (!strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), `"stop_reason":"tool_use"`)) {
			t.Fatalf("valid tool rejected: %s", w.Body.String())
		}
		if !valid && (strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), `"type":"error"`)) {
			t.Fatalf("invalid tool reported success: %s", w.Body.String())
		}
	}
}

// 顶层不支持块（document / search_result / tool_reference 等）用文字占位，而不是整条 400。
func TestMessagesTopLevelUnsupportedBlocksBecomePlaceholders(t *testing.T) {
	body := `{"model":"cn:fixture","max_tokens":64,"messages":[{"role":"user","content":[
	  {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}},
	  {"type":"search_result","content":[{"type":"text","text":"citation"}]},
	  {"type":"tool_reference","tool_name":"Read"},
	  {"type":"text","text":"keep me"}]}]}`
	h, out, calls, _ := messagesFixture(t, sseOK)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-API-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || *calls != 1 {
		t.Fatalf("top-level unsupported blocks rejected: %d %d %s", w.Code, *calls, w.Body)
	}
	encoded, _ := json.Marshal(*out)
	sent := string(encoded)
	if !strings.Contains(sent, "unsupported content block: document") ||
		!strings.Contains(sent, "unsupported content block: tool_reference") ||
		!strings.Contains(sent, "keep me") {
		t.Fatalf("placeholders or kept text missing: %s", sent)
	}
}

// 跨轮复用同一个工具调用 id 是合法的；只有同轮重复或上一轮尚未配对才拒绝。
func TestMessagesToolIDsMayRepeatAcrossTurns(t *testing.T) {
	body := `{"model":"cn:fixture","max_tokens":64,"messages":[
	  {"role":"user","content":"first"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"path":"a"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"A"}]},
	  {"role":"user","content":"second"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"path":"b"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"B"},{"type":"text","text":"done"}]}]}`
	h, _, calls, _ := messagesFixture(t, sseOK)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-API-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || *calls != 1 {
		t.Fatalf("reused tool id rejected: %d %d %s", w.Code, *calls, w.Body)
	}
}

// 空工具结果不再送空 text part（上游常见 400 形状），回退成空串。
func TestMessagesEmptyToolResultBecomesEmptyString(t *testing.T) {
	body := `{"model":"cn:fixture","max_tokens":64,"messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":""}]},
	  {"role":"user","content":"go on"}]}`
	h, out, calls, _ := messagesFixture(t, sseOK)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	r.Header.Set("X-API-Key", "fixture-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || *calls != 1 {
		t.Fatalf("empty tool result rejected: %d %d %s", w.Code, *calls, w.Body)
	}
	encoded, _ := json.Marshal(*out)
	sent := string(encoded)
	if strings.Contains(sent, `"text":""`) {
		t.Fatalf("empty text part was forwarded: %s", sent)
	}
}

// 运行约定必须追加到第一条 system（part 数组形态也一样），不能另起一条 system。
func TestApplyActNoteAppendsToArraySystem(t *testing.T) {
	body := []byte(`{"model":"cn:fixture","messages":[{"role":"system","content":[{"type":"text","text":"You are Claude Code."}]},{"role":"user","content":"hi"}]}`)
	out := applyActNote(body, "Tool execution protocol: act, do not narrate.", true)
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	systems, noteIndex := 0, -1
	for index, message := range parsed.Messages {
		if message["role"] != "system" {
			continue
		}
		systems++
		if index != 0 {
			t.Fatalf("system message is not first: %v", parsed.Messages)
		}
		parts, _ := message["content"].([]any)
		first, _ := parts[0].(map[string]any)
		if first["text"] != "You are Claude Code." {
			t.Fatalf("client system prompt moved: %v", message["content"])
		}
		for partIndex, raw := range parts {
			part, _ := raw.(map[string]any)
			if text, _ := part["text"].(string); strings.Contains(text, "Tool execution protocol") {
				noteIndex = partIndex
			}
		}
	}
	if systems != 1 || noteIndex != 1 {
		t.Fatalf("systems=%d noteIndex=%d messages=%v", systems, noteIndex, parsed.Messages)
	}
}

// Claude Code 2.1.283 遇到它不认识的模型名（中转站常态，例如 global:deepseek-v4.1-flash[1M]）
// 会把 Environment 段单独作为 role=system 的消息放进 messages。官方端点容忍这种写法，网关
// 早期版本按「只能 user/assistant」整条 400（用户看到的就是 messages[1].role must be…），
// 这组用例守住「并回系统提示，而不是拒绝」。
func TestMessagesSystemRoleFoldsIntoSystemPrompt(t *testing.T) {
	cases := map[string]struct {
		input  string
		wanted []string
	}{
		"trailing with system field": {
			input:  `{"model":"cn:fixture","max_tokens":64,"system":[{"type":"text","text":"top instructions"}],"messages":[{"role":"user","content":"hello"},{"role":"system","content":[{"type":"text","text":"# Environment\ncwd is here"}]}]}`,
			wanted: []string{"top instructions", "cwd is here"},
		},
		"leading without system field": {
			input:  `{"model":"cn:fixture","max_tokens":64,"messages":[{"role":"system","content":"no system field"},{"role":"user","content":"hello"}]}`,
			wanted: []string{"no system field"},
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			chat, _, _, err := messagesToChat([]byte(test.input))
			if err != nil {
				t.Fatalf("system-role message rejected: %v", err)
			}
			if err := validateChatRequest(chat); err != nil {
				t.Fatalf("folded request no longer validates: %v", err)
			}
			var object map[string]any
			if err := json.Unmarshal(chat, &object); err != nil {
				t.Fatal(err)
			}
			messages := object["messages"].([]any)
			if len(messages) != 2 {
				t.Fatalf("system message was not folded into the prompt: %s", chat)
			}
			system, _ := messages[0].(map[string]any)
			if system["role"] != "system" {
				t.Fatalf("system prompt is not the first message: %s", chat)
			}
			if messages[1].(map[string]any)["role"] != "user" {
				t.Fatalf("user turn lost: %s", chat)
			}
			encoded := string(chat)
			for _, wanted := range test.wanted {
				if !strings.Contains(encoded, wanted) {
					t.Fatalf("system text %q lost: %s", wanted, chat)
				}
			}
		})
	}
}

func TestMessagesToolRoleBecomesUserTurn(t *testing.T) {
	input := `{"model":"cn:fixture","max_tokens":64,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},{"role":"tool","content":[{"type":"tool_result","tool_use_id":"t1","content":"output"}]}]}`
	chat, _, _, err := messagesToChat([]byte(input))
	if err != nil {
		t.Fatalf("tool-role message rejected: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(chat, &object); err != nil {
		t.Fatal(err)
	}
	messages := object["messages"].([]any)
	if len(messages) != 3 || messages[2].(map[string]any)["role"] != "tool" || messages[2].(map[string]any)["tool_call_id"] != "t1" {
		t.Fatalf("tool result pairing lost: %s", chat)
	}
}

func TestMessagesStillRejectsUnusableRoles(t *testing.T) {
	for name, input := range map[string]string{
		"unknown role": `{"model":"cn:fixture","max_tokens":64,"messages":[{"role":"user","content":"x"},{"role":"observer","content":"y"}]}`,
		"system only":  `{"model":"cn:fixture","max_tokens":64,"messages":[{"role":"system","content":"only system"}]}`,
	} {
		if _, _, _, err := messagesToChat([]byte(input)); err == nil {
			t.Fatalf("%s must stay rejected", name)
		}
	}
}

// TestMessagesAcceptBuiltinToolDeclarations Anthropic Messages 路径必须与
// Chat / Responses 一致：客户端默认携带的服务端内置工具（Claude Code 的 WebSearch
// 就是 web_search_20250305）只接受声明、不转发，而不是整条请求 400。
//
// 2026-09-29 实测：Claude Code 一用联网搜索就整条会话不可用，报
// `400 server tools are not supported; provide client tools with input_schema`。
func TestMessagesAcceptBuiltinToolDeclarations(t *testing.T) {
	input := `{"model":"cn:fixture","max_tokens":1024,` +
		`"messages":[{"role":"user","content":"search something"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5},` +
		`{"name":"read","input_schema":{"type":"object"}}]}`
	chat, _, _, err := messagesToChat([]byte(input))
	if err != nil {
		t.Fatalf("内置工具声明不应让整条请求失败: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(chat, &object); err != nil {
		t.Fatal(err)
	}
	tools, _ := object["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("上游只应看到 1 个客户端工具，实际 %d: %s", len(tools), chat)
	}
	function, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read" {
		t.Fatalf("保留的应是客户端自定义工具: %s", chat)
	}
}

// TestMessagesBuiltinOnlyToolsOmitField 只有内置工具时不得留空 tools 数组：
// 「没有可调用工具」用省略字段表达，空数组对上游行为没有保证。
func TestMessagesBuiltinOnlyToolsOmitField(t *testing.T) {
	input := `{"model":"cn:fixture","max_tokens":1024,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	chat, _, _, err := messagesToChat([]byte(input))
	if err != nil {
		t.Fatalf("仅内置工具时不应报错: %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(chat, &object); err != nil {
		t.Fatal(err)
	}
	if _, exists := object["tools"]; exists {
		t.Fatalf("仅内置工具时应省略 tools 字段: %s", chat)
	}
}
