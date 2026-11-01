// outbound_fidelity_test.go 出站头与官方客户端的逐字段比对。
//
// 目的：把「严格伪装」变成可回归的断言。基线来自真实抓包（不入库原文，
// 只保留字段名与取值规则）：
//
//	CN     _wb_capture2 005747_0101 / 005755_0150，_wb_capture_new 033（V5.9.3）
//	global _wb_capture2 005742_0068，_wb_capture_new 039（V5.9.3）
//
// 断言两条：
//  1. 官方发出的每个业务头，网关都必须发（缺失即指纹差异）；
//  2. 网关不得发出官方没有的头（多发即指纹差异）。
//
// 传输层自动头（Host/Content-Length/Connection/Accept-Encoding）由 HTTP 栈生成，
// 不参与比对；Content-Encoding 单独断言必须为 gzip。
package upstream

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// officialChatHeaders 官方 chat 请求携带的全部业务头（小写）。
// 两个 realm 完全相同——2026-10-29 V5.9.3 抓包 033/039 逐字段一致，
// 唯一按域变化的是取值（X-Domain / X-Private-Data / UA 平台段）。
var officialChatHeaders = []string{
	"accept",
	"acp-connection-id",
	"authorization",
	"b3",
	"baggage",
	"content-type",
	"traceparent",
	"user-agent",
	"x-agent-intent",
	"x-agent-purpose",
	"x-agent-runtime",
	"x-agent-type",
	"x-b3-parentspanid",
	"x-b3-sampled",
	"x-b3-spanid",
	"x-b3-traceid",
	"x-codebuddy-request",
	"x-conversation-id",
	"x-conversation-message-id",
	"x-conversation-request-id",
	"x-device-token", // 无 token 时允许缺（见下方豁免）
	"x-domain",
	"x-ide-name",
	"x-ide-type",
	"x-ide-version",
	"x-private-data",
	"x-product",
	"x-request-id",
	"x-requested-with",
	"x-root-request-id",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"x-trace-id",
	"x-user-id",
}

// deviceTokenOptional 无 device token 时必须整头省略（发空值更可疑）。
var deviceTokenOptional = map[string]bool{"x-device-token": true}

func TestOutboundHeadersMatchOfficialBaseline(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *auth.Auth
	}{
		{"cn", &auth.Auth{AccessToken: "at", UID: "11111111-2222-3333-4444-555555555555", Domain: "www.codebuddy.cn"}},
		{"global", &auth.Auth{AccessToken: "at", UID: "11111111-2222-3333-4444-555555555555", Domain: "www.workbuddy.ai"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t)
			c := &Client{}
			c.ChatHeaders(req, tc.auth, "", ChatMeta{GatewayRequestID: "3f2b1c4d5e6f708192a3b4c5d6e7f809"})

			got := map[string]bool{}
			for k := range req.Header {
				got[strings.ToLower(k)] = true
			}

			// 1) 官方有、网关必须也有
			for _, want := range officialChatHeaders {
				if got[want] {
					continue
				}
				if deviceTokenOptional[want] {
					continue
				}
				t.Errorf("[%s] 缺少官方头 %q（网关未发，属指纹差异）", tc.name, want)
			}

			// 2) 网关不得多发官方没有的头
			allowed := map[string]bool{}
			for _, k := range officialChatHeaders {
				allowed[k] = true
			}
			extra := make([]string, 0, 4)
			for k := range got {
				if !allowed[k] {
					extra = append(extra, k)
				}
			}
			sort.Strings(extra)
			if len(extra) > 0 {
				t.Errorf("[%s] 多发官方没有的头：%v", tc.name, extra)
			}
		})
	}
}

// TestOutboundDomainMatchesChatHost X-Domain 必须等于 chat 上游域，不是登录域。
func TestOutboundDomainMatchesChatHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		auth   *auth.Auth
		want   string
	}{
		{"cn-login-domain", &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.codebuddy.cn"}, "www.workbuddy.cn"},
		{"cn-empty-domain", &auth.Auth{UID: "u", AccessToken: "at"}, "www.workbuddy.cn"},
		{"cn-workbuddy-domain", &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.workbuddy.cn"}, "www.workbuddy.cn"},
		{"global", &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.workbuddy.ai"}, "www.workbuddy.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t)
			c := &Client{}
			c.ChatHeaders(req, tc.auth, "", ChatMeta{})
			if got := req.Header.Get("X-Domain"); got != tc.want {
				t.Errorf("X-Domain = %q want %q", got, tc.want)
			}
		})
	}
}

// TestOutboundPrivateDataByRealm X-Private-Data 按 realm 取值（V5.9.3 实证）。
func TestOutboundPrivateDataByRealm(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *auth.Auth
		want string
	}{
		{"cn", &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.codebuddy.cn"}, "false"},
		{"global", &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.workbuddy.ai"}, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(t)
			c := &Client{}
			c.ChatHeaders(req, tc.auth, "", ChatMeta{})
			if got := req.Header.Get("X-Private-Data"); got != tc.want {
				t.Errorf("X-Private-Data = %q want %q", got, tc.want)
			}
		})
	}
}

// TestOutboundBaggageMatchesOfficialShape baggage 形态必须与官方一致：
// buddyId=,buddyName=,codebuddy.session_id=<convID>,codebuddy.conversation_request_id=<convReqID>
func TestOutboundBaggageMatchesOfficialShape(t *testing.T) {
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, &auth.Auth{UID: "u", AccessToken: "at", Domain: "www.codebuddy.cn"}, "",
		ChatMeta{GatewayRequestID: "3f2b1c4d5e6f708192a3b4c5d6e7f809"})

	bag := req.Header.Get("baggage")
	if !strings.HasPrefix(bag, "buddyId=,buddyName=,codebuddy.session_id=") {
		t.Fatalf("baggage 前缀不符官方形态：%q", bag)
	}
	if !strings.Contains(bag, ",codebuddy.conversation_request_id=") {
		t.Fatalf("baggage 缺 conversation_request_id 段：%q", bag)
	}
	// session_id 段必须与 X-Conversation-ID 同值（官方 033 实证）
	convID := req.Header.Get("X-Conversation-ID")
	if !strings.Contains(bag, "codebuddy.session_id="+convID) {
		t.Errorf("baggage session_id 与 X-Conversation-ID 不一致：bag=%q conv=%q", bag, convID)
	}
	convReqID := req.Header.Get("X-Conversation-Request-ID")
	if !strings.Contains(bag, "codebuddy.conversation_request_id="+convReqID) {
		t.Errorf("baggage conversation_request_id 与 X-Conversation-Request-ID 不一致：bag=%q req=%q", bag, convReqID)
	}
}

// TestOutboundIDsAreUUIDv7 出站 ID 必须是 UUIDv7（前 48 位 = 毫秒时间戳）。
// 官方全部 ID 都是 v7；纯随机 hex 解码后是 2001~8000 年间的乱值，属硬指纹。
func TestOutboundIDsAreUUIDv7(t *testing.T) {
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, &auth.Auth{UID: "u", AccessToken: "at"}, "", ChatMeta{})

	for _, hdr := range []string{
		"X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Request-ID", "X-Root-Request-ID", "X-Trace-ID", "X-B3-TraceId",
	} {
		v := req.Header.Get(hdr)
		if len(v) != 32 {
			t.Errorf("%s = %q want 32 hex", hdr, v)
			continue
		}
		// 版本位：第 13 个 hex（下标 12）必须是 '7'
		if v[12] != '7' {
			t.Errorf("%s = %q 版本位=%c want '7'（UUIDv7）", hdr, v, v[12])
		}
		// 变体位：下标 16 必须是 8/9/a/b
		if !strings.ContainsRune("89ab", rune(v[16])) {
			t.Errorf("%s = %q 变体位=%c want 8/9/a/b", hdr, v, v[16])
		}
	}
	// acp-connection-id 是带横线形态，去掉横线后同样要是 v7
	acp := strings.ReplaceAll(req.Header.Get("acp-connection-id"), "-", "")
	if len(acp) == 32 && acp[12] != '7' {
		t.Errorf("acp-connection-id = %q 版本位=%c want '7'", acp, acp[12])
	}
}

// TestOutboundAcceptIsPlainJSON 官方 chat Accept 恒为 application/json。
func TestOutboundAcceptIsPlainJSON(t *testing.T) {
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, &auth.Auth{UID: "u", AccessToken: "at"}, "", ChatMeta{})
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q want application/json", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "" {
		t.Errorf("Accept-Language = %q want empty（官方 chat 不发）", got)
	}
}

// TestGzipBodyRoundTrip gzipBody 产出可解压且内容一致。
func TestGzipBodyRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"x","messages":[{"role":"user","content":"` + strings.Repeat("a", 5000) + `"}]}`)
	gz, err := gzipBody(raw)
	if err != nil {
		t.Fatalf("gzipBody: %v", err)
	}
	if len(gz) >= len(raw) {
		t.Errorf("压缩后 %d 字节未小于原文 %d（可压缩内容应显著变小）", len(gz), len(raw))
	}
	req, _ := http.NewRequest("POST", "https://x.invalid", nil)
	req.Body = http.NoBody
	_ = req
	// 用同一辅助解压回来核对
	plain := gunzipForTest(t, gz)
	if string(plain) != string(raw) {
		t.Errorf("解压后与原文不一致：%d vs %d 字节", len(plain), len(raw))
	}
}
