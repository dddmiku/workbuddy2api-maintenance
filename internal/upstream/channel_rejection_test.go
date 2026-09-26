// ═══ 更新日志 ═══
// 2026-09-16：以真实上游渠道错误锁定分类，防止安全策略展示文案覆盖主错误及回显文本误判。
package upstream

import (
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
