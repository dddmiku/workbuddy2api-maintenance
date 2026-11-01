package upstream

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// globalUAString global realm 默认出站 UA。
//
// 2026-10-29 双端抓包实测（V5.9.3）：
//
//	global → `WorkBuddy/5.9.3 workbuddy-ai/5.9.3 CLI/2.165.0`
//	CN     → `WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0`
//
// 第二段按 realm 分流：国际版用**全小写连字符** `workbuddy-ai`，国内版沿用
// `WorkBuddy`。此前两 realm 同形（2026-09-17 对 5.5.2 抓包的结论）已被实测推翻。
const globalUAString = "WorkBuddy/" + defaultClientVersion + " workbuddy-ai/" + defaultClientVersion + " CLI/" + defaultCliVersion

// TestChatHeadersGlobalRealm global 账号的 chat 请求头对齐 intl 三件套：
//  1. UA 用官方国际版实测形态 `WorkBuddy/<v> WorkBuddy/<v> CLI/<cli>`；
//  2. X-No-Enterprise-Id: 1（个人账号无企业 ID，显式声明）；
//  3. X-Domain: www.workbuddy.ai（显式声明国际版域）。
//
// 与 CN 的差异是修复 global 403 code 11140 "request illegal" 风控的关键（
// ANALYSIS-global-chat-solutions.md）。
func TestChatHeadersGlobalRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"} // global 账号
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); got != globalUAString {
		t.Errorf("global UA = %q want %q", got, globalUAString)
	}
	// 2026-10-31 按官方抓包修正：官方 global chat 不发 X-No-Enterprise-Id。
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("global X-No-Enterprise-Id = %q want empty（官方 global chat 不发）", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global X-Domain = %q want %q", got, "www.workbuddy.ai")
	}
	// global 账号不落 CN 侧的 X-Enterprise-Id（企业头无意义，只发 No-* 声明）。
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global X-Enterprise-Id = %q want empty", got)
	}
}

// TestChatHeadersCNRealm CN 账号头：UA 用 `WorkBuddy` 平台段、不注入 global 专属的
// X-No-Enterprise-Id；EnterpriseID 非空走 X-Enterprise-Id 分支。
//
// 2026-10-31 按官方抓包修正断言：CN 桌面端 chat 请求**不发** X-No-Enterprise-Id，
// 且 X-Domain 恒为 www.workbuddy.cn（chat 上游域，不是登录域）。原断言
// 「X-Domain 空 → X-No-Department-Info」是网关自造形态，官方从不产生。
func TestChatHeadersCNRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c1", EnterpriseID: "e1"} // CN 企业账号 + 无 domain
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("CN UA = %q must NOT contain WorkBuddy AI (want %q)", got, defaultUAString)
	}
	if got := req.Header.Get("User-Agent"); got != defaultUAString {
		t.Errorf("CN UA = %q want %q", got, defaultUAString)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "e1" {
		t.Errorf("CN X-Enterprise-Id = %q want e1", got)
	}
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("CN X-No-Enterprise-Id = %q want empty (官方 CN chat 不发该头)", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Errorf("CN X-Domain = %q want www.workbuddy.cn (chat 上游域，非登录域)", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("CN X-No-Department-Info = %q want empty (X-Domain supersedes)", got)
	}
}

// TestChatHeadersGlobalStrongOverride global 账号（domain 标记 global）即便带
// EnterpriseID（异常数据）也发国际客户端形态头：X-No-Enterprise-Id=1 +
// X-Domain=www.workbuddy.ai，不落 CN 侧企业头（覆盖注入顺序：global 分支先于
// 既有分支的强覆盖语义）。
func TestChatHeadersGlobalStrongOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g2", EnterpriseID: "should-be-ignored", Domain: "www.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	// 官方 global chat 不发 X-No-Enterprise-Id（2026-10-31 抓包实证）。
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("global(virtual enterprise) X-No-Enterprise-Id = %q want empty", got)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global(virtual enterprise) X-Enterprise-Id = %q want empty", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(virtual enterprise) X-Domain = %q want www.workbuddy.ai", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("global(virtual enterprise) X-No-Department-Info = %q want empty (X-Domain supersedes)", got)
	}
}

// TestChatHeadersGlobalWithConfiguredDomain global 账号即便显式配了登录 domain，
// chat 出站 X-Domain 也固定为 www.workbuddy.ai（国际客户端形态，不回退到会话原值）。
func TestChatHeadersGlobalWithConfiguredDomain(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g3", Domain: "login.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(with custom domain) X-Domain = %q want www.workbuddy.ai", got)
	}
}

// TestChatHeadersCNNoEnterpriseZeroRegression CN 无企业账号：X-Domain 仍归一为
// www.workbuddy.cn（chat 上游域），不发 X-No-Enterprise-Id（官方 CN chat 无此头）。
//
// 2026-10-31 按官方抓包修正：原断言「X-No-Enterprise-Id=1 + X-No-Department-Info=1 +
// 无 X-Domain」是网关自造形态，官方 CN 桌面端从不这样发。
func TestChatHeadersCNNoEnterpriseZeroRegression(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c2"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("CN(no enterprise) X-No-Enterprise-Id = %q want empty (官方 CN chat 不发)", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Errorf("CN(no enterprise) X-Domain = %q want www.workbuddy.cn", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("CN(no enterprise) X-No-Department-Info = %q want empty", got)
	}
}

// TestDefaultWorkBuddyUAForGlobal UA 单元级断言：第二段按 realm 分流
// （global → `workbuddy-ai`，cn → `WorkBuddy`），版本覆盖仍生效。
func TestDefaultWorkBuddyUAForGlobal(t *testing.T) {
	c := &Client{}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{}); got != defaultUAString {
		t.Errorf("defaultWorkBuddyUAFor(cn) = %q want %q", got, defaultUAString)
	}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != globalUAString {
		t.Errorf("defaultWorkBuddyUAFor(global) = %q want %q", got, globalUAString)
	}
	// version 覆盖仍生效：平台段跟随 clientVersion。
	c2 := &Client{ClientVersion: "6.0.0"}
	if got := c2.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != "WorkBuddy/6.0.0 workbuddy-ai/6.0.0 CLI/"+defaultCliVersion {
		t.Errorf("defaultWorkBuddyUAFor(global, v6) = %q", got)
	}
}

// mustRequest 构造一个无 body 的 POST 请求（测试 ChatHeaders 用）。
func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://chat.example/console/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}
