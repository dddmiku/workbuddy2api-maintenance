package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCommonHeadersCodeBuddyRequest 所有出站请求统一注入
// X-CodeBuddy-Request: 1（官方客户端风控闸门头，D1）。
// chat/billing/refresh 三类出站均覆盖。
func TestCommonHeadersCodeBuddyRequest(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{}

	// chat 路径
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("chat X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// billing 路径
	req2 := mustRequest(t)
	c.BillingHeaders(req2, a)
	if got := req2.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("billing X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// refresh 路径
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, a)
	if got := req3.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("refresh X-CodeBuddy-Request = %q want %q", got, "1")
	}
}

// TestRefreshHeadersAuthRefreshSource refresh 出站 X-Auth-Refresh-Source 对齐
// 官方客户端 refresh 渠道标识为 "plugin"（D3，原值 "workbuddy" 与官方不一致）。
func TestRefreshHeadersAuthRefreshSource(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	req := mustRequest(t)
	c := &Client{}
	c.RefreshHeaders(req, a)

	if got := req.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
		t.Errorf("X-Auth-Refresh-Source = %q want %q", got, "plugin")
	}
}

// TestCommonHeadersAcceptLanguageByRealm Accept-Language 只在非 chat 路径发（D5）：
// CN 账号 zh-CN，global 账号 en-US。
//
// 2026-10-31 按官方抓包修正：**chat 出站不发 Accept-Language**——官方桌面端 chat
// 走 @openai Node SDK，不带这个浏览器语义的头（_wb_capture2 005747_0101 /
// _wb_capture_new 033 均无）。此前网关在 chat 上发它，是官方没有的指纹。
func TestCommonHeadersAcceptLanguageByRealm(t *testing.T) {
	c := &Client{}

	// CN 账号 chat：不发
	cnA := &auth.Auth{AccessToken: "at", UID: "c1"}
	req := mustRequest(t)
	c.ChatHeaders(req, cnA, "", ChatMeta{})
	if got := req.Header.Get("Accept-Language"); got != "" {
		t.Errorf("CN chat Accept-Language = %q want empty (官方 chat 不发)", got)
	}

	// global 账号 chat：不发
	glA := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	req2 := mustRequest(t)
	c.ChatHeaders(req2, glA, "", ChatMeta{})
	if got := req2.Header.Get("Accept-Language"); got != "" {
		t.Errorf("global chat Accept-Language = %q want empty (官方 chat 不发)", got)
	}

	// CN 账号 refresh：发 zh-CN（网页 XHR 链路，官方确实带）
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, cnA)
	if got := req3.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("CN refresh Accept-Language = %q want %q", got, "zh-CN")
	}

	// global 账号 refresh：发 en-US
	req4 := mustRequest(t)
	c.RefreshHeaders(req4, glA)
	if got := req4.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("global refresh Accept-Language = %q want %q", got, "en-US")
	}
}

// TestAcceptHeaderStreamVsNonStream Accept 头分流式/非流式（D6）：
// **官方 chat 出站的 Accept 恒为 application/json**（2026-10-31 六份抓包 6/6），
// billing/refresh 同样是 application/json（去掉宽松的 text/plain, */*）。
// SSE 由请求体 stream=true 决定，与 Accept 无关。
func TestAcceptHeaderStreamVsNonStream(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}

	// chat：application/json（官方实测，不发 text/event-stream）
	chatReq := mustRequest(t)
	c.ChatHeaders(chatReq, a, "", ChatMeta{})
	if got := chatReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("chat Accept = %q want %q", got, "application/json")
	}

	// billing 非流式：application/json
	bReq := mustRequest(t)
	c.BillingHeaders(bReq, a)
	if got := bReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("billing Accept = %q want %q", got, "application/json")
	}

	// refresh 非流式：application/json
	rReq := mustRequest(t)
	c.RefreshHeaders(rReq, a)
	if got := rReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("refresh Accept = %q want %q", got, "application/json")
	}
}
