// ═══ 更新日志 ═══
// 2026-09-16：以真实上游渠道错误锁定分类，防止安全策略展示文案覆盖主错误及回显文本误判。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyExplicitChannelRejection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"observed 400", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","displayMsg":"Blocked by security policy"}`, "channel_rejected"},
		{"business error", 200, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, "channel_rejected"},
		{"case and punctuation", 403, `{"msg":"  ILLEGAL API INVOCATION FROM AN UNAPPROVED CHANNEL.  "}`, "channel_rejected"},
		{"error envelope", 400, `{"error":{"code":11128,"message":"Illegal API invocation from an unapproved channel"}}`, "channel_rejected"},
		{"plain message", 400, `Illegal API invocation from an unapproved channel`, "channel_rejected"},
		{"content remains content", 400, `{"code":11128,"msg":"blocked by security policy: NSFW content"}`, "content_blocked"},
		{"code alone is not channel", 400, `{"code":11128,"msg":"unknown rejection"}`, "client"},
		{"generic invocation is not a content finding", 400, `{"code":11128,"msg":"illegal api invocation"}`, "client"},
		{"quoted request is not a channel finding", 400, `{"code":11128,"msg":"blocked by security policy: NSFW content","request_excerpt":"Illegal API invocation from an unapproved channel"}`, "content_blocked"},
		{"display text is not the primary reason", 400, `{"msg":"invalid request","displayMsg":"Illegal API invocation from an unapproved channel"}`, "client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.status, c.body).String(); got != c.want {
				t.Fatalf("Classify=%s want=%s for %s", got, c.want, c.body)
			}
		})
	}
}

// 2026-09-26 实测：Claude Code 系统提示首行被上游按未批准渠道拒绝，断词后可通过。
func TestNeutralizeChannelTriggerCoversClaudeCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"claude code sentence", `{"messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."}]}`, true},
		{"claude code fingerprint only", `{"messages":[{"role":"system","content":"Claude Code, Anthropic's official CLI for Claude"}]}`, true},
		{"plain claude code mention", `{"messages":[{"role":"user","content":"Claude Code 是什么？"}]}`, false},
		{"official cli alone", `{"messages":[{"role":"system","content":"Anthropic's official CLI for Claude"}]}`, false},
		{"codex sentence", `{"messages":[{"role":"system","content":"Codex CLI is an open source project led by OpenAI."}]}`, true},
		{"content array", `{"messages":[{"role":"system","content":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}]}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := NeutralizeChannelTrigger([]byte(tc.body))
			if changed != tc.want {
				t.Fatalf("changed=%v want=%v out=%s", changed, tc.want, out)
			}
			if changed && !strings.Contains(string(out), "\u200b") {
				t.Fatal("no word break was inserted")
			}
		})
	}
}

// 分级升级：未知客户端的归属句也能在中和后通过，且不动结构性字符串。
func TestNeutralizeChannelTriggerEscalationLevels(t *testing.T) {
	unknownClient := `{"model":"m","messages":[{"role":"system","content":"You are Foobar, the official CLI for Baz. Always answer briefly."},{"role":"user","content":"hi"}]}`
	if _, changed := NeutralizeChannelTriggerAt([]byte(unknownClient), channelLevelFingerprint); changed {
		t.Fatal("level 0 cannot know an unlisted client")
	}
	out, changed := NeutralizeChannelTriggerAt([]byte(unknownClient), channelLevelAttribution)
	if !changed || !strings.Contains(string(out), "\u200b") {
		t.Fatal("attribution sentence was not neutralized at level 1")
	}
	if strings.Contains(string(out), "Always answer briefly.\u00a0") {
		t.Fatal("unrelated sentence should stay intact")
	}
	plain := `{"model":"m","messages":[{"role":"user","content":"write a function that reverses a list"}]}`
	if _, changed := NeutralizeChannelTriggerAt([]byte(plain), channelLevelAttribution); changed {
		t.Fatal("plain content must stay untouched at level 1")
	}
	structured := `{"model":"m","tool_choice":"auto","tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"mode":{"enum":["fast","safe"]}}}}}],"messages":[{"role":"assistant","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"Read","arguments":"{\"mode\":\"fast\"}"}}]},{"role":"tool","tool_call_id":"call_abc","content":"done"}]}`
	out, changed = NeutralizeChannelTriggerAt([]byte(structured), channelLevelAll)
	if !changed {
		t.Fatal("level 2 must always break some message text")
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	tools := parsed["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tools["name"] != "Read" {
		t.Fatalf("tool name was rewritten: %v", tools["name"])
	}
	call := parsed["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_abc" {
		t.Fatalf("tool call id was rewritten: %v", call["id"])
	}
	if call["function"].(map[string]any)["arguments"] != `{"mode":"fast"}` {
		t.Fatalf("tool arguments were rewritten: %v", call["function"])
	}
	if !strings.Contains(parsed["messages"].([]any)[1].(map[string]any)["content"].(string), "\u200b") {
		t.Fatal("message content was not broken at level 2")
	}
}
