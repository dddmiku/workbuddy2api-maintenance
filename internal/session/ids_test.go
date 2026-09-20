package session

import (
	"strings"
	"testing"
)

// TestResolveConversationID 覆盖 conversationId 提取的 snake/camel/缺失三态：
//   - metadata.conversation_id / metadata.conversationId → 取值
//   - 顶层 conversation_id / conversationId → 取值（snake 优先同 ExtractKey）
//   - 缺失 / 只有 user_id → ""（会话头族语义只认对话 ID，绝不回落 user_id）
func TestResolveConversationID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveConversationID([]byte(c.body)); got != c.want {
				t.Errorf("ResolveConversationID(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestNewMessageIDFormat 32 位 hex 且不为空、两次调用大概率不同（随机性冒烟）。
func TestNewMessageIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

// TestRequestIDForKeyDerivation 同 key 恒稳定、异 key 各不同、空 key 每次新值
// （纯派生实现，无包级缓存，测试间天然隔离）。
func TestRequestIDForKeyDerivation(t *testing.T) {
	k1a := RequestIDForKey("conv-a")
	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}
	k2 := RequestIDForKey("conv-b")
	if k1a == k2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// 空 key：每次调用生成新值（无会话则无"会话内稳定"语义）。
	e1 := RequestIDForKey("")
	e2 := RequestIDForKey("")
	if e1 == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", e1)
	}
	// 稳定值自身也须是 32 hex（可作 B3 TraceId 直接使用）。
	for _, id := range []string{k1a, k2, e1} {
		if len(id) != 32 {
			t.Errorf("RequestIDForKey value %q len=%d want 32", id, len(id))
		}
	}
}

// TestTurnKeyExtraction 轮级兜底键的提取：取**最后一条** user 消息的「序号+文本」，
// 轮内追加 assistant/tool 消息不改变键；无 user / 无文本 / 坏 JSON 一律空串。
func TestTurnKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"你好"}]}`, "u0:你好"},
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "u2:第二问"},
		// agent 多步：轮内追加 assistant/tool 消息，末条 user 位置与内容不变 → 同键。
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"任务"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"结果"}]}`, "u0:任务"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:看图"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation 轮级 ID 是纯派生：同键恒同值、异键异值、空键每次新值、
// 形态恒 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	a1 := TurnRequestID("u0:同一个问题")
	a2 := TurnRequestID("u0:同一个问题")
	if a1 != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", a1, a2)
	}
	if b := TurnRequestID("u0:另一个问题"); b == a1 {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// 同文本但序号不同（不同轮里内容相同的提问）也要分开。
	if c := TurnRequestID("u2:同一个问题"); c == a1 {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// 空键：无轮可聚合 → 每次新值（保持原有请求级独立行为）。
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}
	for _, id := range []string{a1, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}

// TestContentKeyDerivation 对话级回退键：取第一条 user 消息文本并哈希。
// 回归背景：narrafork 的 /v1/chat/completions 请求体顶层只有
// model/messages/stream/stream_options/max_tokens/tools/tool_choice/reasoning_effort，
// 没有任何会话标识字段，ExtractKey 恒返回空串 → 会话粘性失效、每轮换号。
func TestContentKeyDerivation(t *testing.T) {
	const narraforkShape = `{"model":"global:deepseek-v4.1-flash","messages":` +
		`[{"role":"system","content":"You are Cascade"},` +
		`{"role":"user","content":"分析这个 APK 的签名校验"},` +
		`{"role":"assistant","content":"好的"}],` +
		`"stream":true,"tools":[],"tool_choice":"auto","reasoning_effort":"max"}`

	key := ContentKey([]byte(narraforkShape))
	if len(key) != 35 || !strings.HasPrefix(key, "c1:") {
		t.Fatalf("ContentKey must be a bounded prefixed digest, got %q", key)
	}
	if ContentKey([]byte(narraforkShape)) != key {
		t.Error("same conversation must derive the same key")
	}

	// 同一对话推进：轮内追加 assistant/tool 消息、首条 user 不变 → 键必须不变
	// （这正是粘性要的「对话级」稳定性）。
	advanced := `{"model":"global:deepseek-v4.1-flash","messages":` +
		`[{"role":"system","content":"You are Cascade"},` +
		`{"role":"user","content":"分析这个 APK 的签名校验"},` +
		`{"role":"assistant","content":"好的"},` +
		`{"role":"tool","content":"结果"},` +
		`{"role":"user","content":"继续"}],"stream":true}`
	if got := ContentKey([]byte(advanced)); got != key {
		t.Errorf("conversation-level key drifted as the turn advanced: %q vs %q", got, key)
	}

	// 首条 user 用数组形态（多模态 parts）也要能取到文本。
	parts := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`
	if got := ContentKey([]byte(parts)); got == "" || got == key {
		t.Errorf("multimodal first user message should derive its own key, got %q", got)
	}

	for _, tc := range []struct{ name, body string }{
		{"no messages", `{"model":"x"}`},
		{"empty messages", `{"messages":[]}`},
		{"system only", `{"messages":[{"role":"system","content":"sys"}]}`},
		{"assistant first", `{"messages":[{"role":"assistant","content":"hi"}]}`},
		{"empty first user", `{"messages":[{"role":"user","content":""},{"role":"user","content":"real"}]}`},
		{"whitespace first user", `{"messages":[{"role":"user","content":"  \n "}]}`},
		{"image only first user", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`},
		{"null content", `{"messages":[{"role":"user","content":null}]}`},
		{"broken json", `{broken`},
		{"empty body", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContentKey([]byte(tc.body)); got != "" {
				t.Errorf("ContentKey(%q) = %q, want empty (no fabricated key)", tc.body, got)
			}
		})
	}

	// 不同对话必须得到不同键，否则两个对话会被钉在同一个号上。
	other := `{"messages":[{"role":"user","content":"另一个完全不同的任务"}]}`
	if ContentKey([]byte(other)) == key {
		t.Error("different conversations must derive different keys")
	}
}
