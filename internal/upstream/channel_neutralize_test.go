// ═══ 更新日志 ═══
// 2026-09-26：未知客户端的渠道拒绝也要逐档升级断词重发；无正文可断时仍为终态。
// 2026-09-17：锁定渠道校验触发句的断词重试：只动命中句、词义不变、每路径只重发一次。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

const channelTriggerSentence = "Codex CLI is an open source project led by OpenAI"

func TestNeutralizeChannelTriggerBreaksSentence(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a coding agent. ` +
		channelTriggerSentence + `. Follow the repository conventions."}]}`)
	out, changed := NeutralizeChannelTrigger(body)
	if !changed {
		t.Fatalf("changed=false, body=%s", out)
	}
	if bytes.Contains(out, []byte(channelTriggerSentence)) {
		t.Fatalf("trigger sentence still intact: %s", out)
	}
	if !bytes.Contains(out, []byte(wafBreakMarker)) {
		t.Fatalf("no break marker inserted: %s", out)
	}
	// 去掉零宽标记后必须逐字还原（不删不改任何字符）。比较解码后的结构：
	// 中性化会经 json.Marshal 重排键序，字节序差异不算内容差异。
	restored := bytes.ReplaceAll(out, []byte(wafBreakMarker), nil)
	var want, got any
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if err := json.Unmarshal(restored, &got); err != nil {
		t.Fatalf("unmarshal restored: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("content changed beyond markers:\n got=%v\nwant=%v", got, want)
	}
	// 句子之外的正文保持原样。
	if !bytes.Contains(out, []byte("Follow the repository conventions.")) {
		t.Fatalf("unrelated text damaged: %s", out)
	}
}

func TestNeutralizeChannelTriggerIgnoresOtherText(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"Codex is a product name here, OpenAI too."}]}`)
	out, changed := NeutralizeChannelTrigger(body)
	if changed || !bytes.Equal(out, body) {
		t.Fatalf("unexpected rewrite: changed=%v body=%s", changed, out)
	}
	if _, changed := NeutralizeChannelTrigger(nil); changed {
		t.Fatalf("empty body must not report a change")
	}
}

func TestNeutralizeChannelTriggerHandlesPunctuationVariant(t *testing.T) {
	// 句末无句号 / 后接中文标点都要命中，靠句子主体匹配。
	for _, tail := range []string{".", "!", "。", "`"} {
		body := []byte(`{"messages":[{"role":"system","content":"` + channelTriggerSentence + tail + `"}]}`)
		out, changed := NeutralizeChannelTrigger(body)
		if !changed {
			t.Fatalf("tail=%q not neutralised", tail)
		}
		if bytes.Contains(out, []byte(channelTriggerSentence)) {
			t.Fatalf("tail=%q sentence survived: %s", tail, out)
		}
	}
}

// TestChatStreamRetriesChannelRejection 断言被 11128 unapproved channel 拒绝后，
// 网关在同一账号、同一路径上用断词后的正文重发一次，且第二次请求成功。
func TestChatStreamRetriesChannelRejection(t *testing.T) {
	var bodies [][]byte
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			if len(bodies) == 1 {
				return jsonResp(400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`), nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Codex. ` +
		channelTriggerSentence + `. Reply briefly."},{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, body, "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
	if len(bodies) != 2 {
		t.Fatalf("attempts=%d want 2 (one neutralised retry)", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(channelTriggerSentence)) {
		t.Fatalf("first attempt should keep the original sentence: %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(channelTriggerSentence)) {
		t.Fatalf("retry still carries the trigger sentence: %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(wafBreakMarker)) {
		t.Fatalf("retry carries no break marker: %s", bodies[1])
	}
}

// TestChatStreamChannelRejectionEscalatesForUnknownClients 未列入词表的客户端也要救回来：
// 第一次原样被拒 → 按档位升级断词重发（通用归属句/正文全量）。
func TestChatStreamChannelRejectionEscalatesForUnknownClients(t *testing.T) {
	attempts := 0
	bodies := [][]byte{}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			if bytes.Contains(raw, []byte(wafBreakMarker)) {
				return jsonResp(200, `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`), nil
			}
			return jsonResp(400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`), nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"You are Foobar, a coding assistant for Baz."},{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, body, "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
	if attempts != 2 {
		t.Fatalf("attempts=%d want 2 (one escalated retry)", attempts)
	}
	if !bytes.Contains(bodies[1], []byte(wafBreakMarker)) {
		t.Fatalf("retry carries no break marker: %s", bodies[1])
	}
}

// TestChatStreamChannelRejectionWithoutAnyTextStaysTerminal 没有可断词的正文时不做无谓重发。
func TestChatStreamChannelRejectionWithoutAnyTextStaysTerminal(t *testing.T) {
	attempts := 0
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return jsonResp(400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`), nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	body := []byte(`{"model":"m"}`)
	_, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, body, "", ChatMeta{})
	if err != nil || status != 400 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want 1 (nothing to neutralize)", attempts)
	}
}

// TestChannelTriggerCoversClaudeCodeBillingHeader 2026-09-28 实测（逐段二分 + 对照）：
// Claude Code 2.1.283 在系统提示开头写入的计费归属头字符串本身命中 11128。
// 整行、只留头名都被拒；"x-anthropic-" 或 "x-anthropic-version:" 通过；断词后 200。
// 它必须落在第 0 档精确指纹里，否则每一轮都要先撞两次墙才升级。
func TestChannelTriggerCoversClaudeCodeBillingHeader(t *testing.T) {
	header := "x-anthropic-billing-header"
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` + header +
		`: cc_version=2.1.283.a2f; cc_entrypoint=sdk-cli;You are a Claude agent."},{"role":"user","content":"hi"}]}`)
	first, changed := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	if !changed {
		t.Fatal("level 0 must neutralise the billing header without any retry")
	}
	if bytes.Contains(first, []byte(header)) {
		t.Fatalf("billing header survived level 0: %s", first)
	}
	if !bytes.Contains(first, []byte(wafBreakMarker)) {
		t.Fatalf("no break marker inserted: %s", first)
	}
	// 其他 anthropic 头不触发（实测通过），断词器不得误伤它们。
	for _, safe := range []string{"x-anthropic-version: 2023-06-01", "x-stainless-lang: js"} {
		clean := []byte(`{"model":"m","messages":[{"role":"system","content":"` + safe + `"},{"role":"user","content":"hi"}]}`)
		if out, changed := NeutralizeChannelTriggerAt(clean, channelLevelFingerprint); changed {
			t.Fatalf("level 0 must not touch %q: %s", safe, out)
		}
	}
}
