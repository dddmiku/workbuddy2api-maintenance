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

// TestNeutralizeBillingHeadersPreemptsTheRetry 计费头必须在发送前就断词：
// 它几乎每个请求都出现，等到被拒再重试等于每个请求白花一次往返。
func TestNeutralizeBillingHeadersPreemptsTheRetry(t *testing.T) {
	// 客户端发来的是干净原文；断词由中性化函数负责。
	header := ccHeader
	body := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"` + header +
		`: cc_version=2.1.283.a2f;"}]},{"role":"user","content":"hi"}]}`)
	out, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("billing header must be neutralised before sending")
	}
	if bytes.Contains(out, []byte(header)) {
		t.Fatalf("billing header survived: %s", out)
	}
	if !bytes.Contains(out, []byte(wafBreakMarker)) {
		t.Fatalf("no break marker inserted: %s", out)
	}
	// 其他内容与结构必须原样保留
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "m" {
		t.Fatalf("unrelated field changed: %v", obj["model"])
	}
	// 不含触发串时不得改动任何字节
	clean := []byte(`{"model":"m","messages":[{"role":"user","content":"x-anthropic-version: 1"}]}`)
	if out, changed := NeutralizeBillingHeaders(clean); changed {
		t.Fatalf("must not touch a body without the trigger: %s", out)
	}
}

// TestTriggerTablesHoldCleanStrings 触发词表里必须是「干净原文」。
//
// 这个检查有实际来历：2026-09-28 那次修复里，触发串在编辑过程中被写成了带零宽空格的
// 形式，于是它永远匹配不到客户端发来的干净原文，第 0 档「命中」却救不回来——现象是
// 每个请求仍然被拒、仍要多花一次往返。断词必须由中性化函数在匹配之后插入。
func TestTriggerTablesHoldCleanStrings(t *testing.T) {
	zwspChar := "\u200b"
	for _, entry := range channelTriggerSentences {
		if strings.Contains(entry, zwspChar) {
			t.Errorf("channelTriggerSentences entry is not a clean literal: %q", entry)
		}
	}
	for _, entry := range billingHeaderTriggers {
		if strings.Contains(entry, zwspChar) {
			t.Errorf("billingHeaderTriggers entry is not a clean literal: %q", entry)
		}
	}
	// 干净串必须能匹配客户端原文并触发断词
	clean := "x-anthropic-billing-header: cc_version=2.1.283.a2f;"
	matched := false
	for _, entry := range billingHeaderTriggers {
		if strings.Contains(clean, entry) {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("billing trigger must match the clean client text %q", clean)
	}
	if _, changed := NeutralizeBillingHeaders([]byte(`{"messages":[{"role":"system","content":"` + clean + `"}]}`)); !changed {
		t.Fatal("pre-neutralisation must fire on the clean client text")
	}
}
