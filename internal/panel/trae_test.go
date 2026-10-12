// ═══ 更新日志 ═══
// 2026-10-13：初版——Trae CN 网页授权登录的回归测试（与 panel/trae_login.py 同口径）。
//
// 重点覆盖「用户粘贴进来的东西形态不定」这一实际风险：完整回调 URL、裸查询串、
// 裸 refreshToken、只带 refreshToken 的回调，四种都要能解析；解析不出来时必须报错，
// 而不是写出一个空账号。
//
// 全部用例**不联网**：兑换接口用桩 RoundTripper 顶替包级客户端 traeExchangeClient。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// traeStubTransport 是替换 traeExchangeClient 用的桩传输层。
type traeStubTransport func(*http.Request) (*http.Response, error)

func (s traeStubTransport) RoundTrip(req *http.Request) (*http.Response, error) { return s(req) }

// traeStubExchange 把包级客户端换成桩，返回「最后一次请求体」的只读快照。
func traeStubExchange(t *testing.T, status int, body string) func() []byte {
	t.Helper()
	original := traeExchangeClient
	var captured []byte
	traeExchangeClient = &http.Client{
		Transport: traeStubTransport(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			captured = raw
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
	t.Cleanup(func() { traeExchangeClient = original })
	return func() []byte { return captured }
}

// traeJWTQuery 造一段带 userJwt/userInfo 的查询串（与 Python 测试里的 jwt_query 等价）。
func traeJWTQuery() string {
	userJWT := `{"Token":"jwt-token","RefreshToken":"refresh-token","ClientID":"ono9krqynydwx5"}`
	userInfo := `{"UserID":"12345","ScreenName":"tester","Region":"CN"}`
	return "userJwt=" + url.QueryEscape(userJWT) + "&userInfo=" + url.QueryEscape(userInfo)
}

// 真实 refreshToken 形态：base64 字符集（含 `+ / =`）+ `.` 分隔的十六进制尾巴。
const traeRealRefreshToken = "sJWEae4XXVAvUxBRp5zMfVrJazeYFO1-JiyaYP3-E==.18dd695b7837d5a9"

// TestTraeParseCallbackFullURL 形态 1：完整回调 URL（带 userJwt），无需兑换。
func TestTraeParseCallbackFullURL(t *testing.T) {
	url := "http://127.0.0.1:8765/authorize?" + traeJWTQuery()
	creds, err := TraeParseCallback(url)
	if err != nil {
		t.Fatalf("完整回调 URL 应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "jwt-token")
	assertTraeCred(t, creds, "refresh_token", "refresh-token")
	assertTraeCred(t, creds, "uid", "12345")
	assertTraeCred(t, creds, "nickname", "tester")
	assertTraeCred(t, creds, "region", "CN")
	// host 缺失时回落到默认兑换主机。
	assertTraeCred(t, creds, "host", TraeExchangeHost)
}

// TestTraeParseCallbackBareQuery 形态 2：只有查询串。
func TestTraeParseCallbackBareQuery(t *testing.T) {
	creds, err := TraeParseCallback(traeJWTQuery())
	if err != nil {
		t.Fatalf("裸查询串应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "jwt-token")
	assertTraeCred(t, creds, "uid", "12345")
}

// TestTraeParseCallbackLeadingQuestionMark 带前导 `?` 的查询串也要能解析。
func TestTraeParseCallbackLeadingQuestionMark(t *testing.T) {
	creds, err := TraeParseCallback("?" + traeJWTQuery())
	if err != nil {
		t.Fatalf("带 ? 的查询串应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "jwt-token")
}

// TestTraeParseCallbackSurroundingWhitespace 前后空白要被 strip 掉。
func TestTraeParseCallbackSurroundingWhitespace(t *testing.T) {
	raw := "  \n http://127.0.0.1:8765/authorize?" + traeJWTQuery() + "\n  "
	creds, err := TraeParseCallback(raw)
	if err != nil {
		t.Fatalf("前后空白应被忽略: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "jwt-token")
}

// TestTraeParseCallbackBareRefreshToken 形态 3：裸 refreshToken（含 base64 的 + / =）。
//
// 这条同时锁住判定顺序：裸 token 里含 `=`，按查询串解析会得到假参数，只有先确认
// 「没有已知凭据字段」才会回退到裸 token 分支并触发兑换。
func TestTraeParseCallbackBareRefreshToken(t *testing.T) {
	body := `{"Result":{"Token":"JWT","UserInfo":"{\"UserID\":\"1\",\"ScreenName\":\"bare\"}"}}`
	captured := traeStubExchange(t, 200, body)

	creds, err := TraeParseCallback(traeRealRefreshToken)
	if err != nil {
		t.Fatalf("裸 refreshToken 应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "JWT")
	assertTraeCred(t, creds, "uid", "1")
	assertTraeCred(t, creds, "nickname", "bare")
	assertTraeCred(t, creds, "refresh_token", traeRealRefreshToken)

	var payload map[string]any
	if err := json.Unmarshal(captured(), &payload); err != nil {
		t.Fatalf("兑换请求体不是合法 JSON: %v", err)
	}
	if got := payload["RefreshToken"]; got != traeRealRefreshToken {
		t.Fatalf("兑换请求里的 RefreshToken = %v，想要 %s", got, traeRealRefreshToken)
	}
	if got := payload["ClientID"]; got != TraeClientID {
		t.Fatalf("兑换请求里的 ClientID = %v，想要 %s", got, TraeClientID)
	}
	if got := payload["ClientSecret"]; got != "-" {
		t.Fatalf("ClientSecret = %v，想要 -", got)
	}
}

// TestTraeParseCallbackRefreshOnlyTriggersExchange 形态 4：只带 refreshToken 的回调。
func TestTraeParseCallbackRefreshOnlyTriggersExchange(t *testing.T) {
	body := `{"Result":{"Token":"exchanged-jwt","RefreshToken":"rotated-refresh",` +
		`"TokenExpireAt":"1791700000","UserInfo":"{\"UserID\":\"999\",\"ScreenName\":\"legacy\"}"}}`
	captured := traeStubExchange(t, 200, body)

	creds, err := TraeParseCallback("http://127.0.0.1:8765/authorize?refreshToken=legacy-refresh")
	if err != nil {
		t.Fatalf("老流程回调应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "exchanged-jwt")
	assertTraeCred(t, creds, "refresh_token", "rotated-refresh")
	assertTraeCred(t, creds, "uid", "999")
	assertTraeCred(t, creds, "token_expire_at", "1791700000")

	var payload map[string]any
	if err := json.Unmarshal(captured(), &payload); err != nil {
		t.Fatalf("兑换请求体不是合法 JSON: %v", err)
	}
	if got := payload["RefreshToken"]; got != "legacy-refresh" {
		t.Fatalf("兑换请求里的 RefreshToken = %v", got)
	}
}

// TestTraeParseCallbackPrefersRefreshTokenOverData 实测回调同时带 data 与 refreshToken：
// 必须优先取 refreshToken（Python 测试钉住的选择）。
func TestTraeParseCallbackPrefersRefreshTokenOverData(t *testing.T) {
	real := "http://127.0.0.1:8765/authorize?isRedirect=true&scope=solo" +
		"&data=" + url.QueryEscape(traeRealRefreshToken) +
		"&refreshToken=" + url.QueryEscape(traeRealRefreshToken)
	body := `{"Result":{"Token":"JWT","UserInfo":"{\"UserID\":\"42\",\"ScreenName\":\"u\"}"}}`
	captured := traeStubExchange(t, 200, body)

	creds, err := TraeParseCallback(real)
	if err != nil {
		t.Fatalf("实测回调应解析成功: %v", err)
	}
	assertTraeCred(t, creds, "access_token", "JWT")
	assertTraeCred(t, creds, "uid", "42")

	var payload map[string]any
	if err := json.Unmarshal(captured(), &payload); err != nil {
		t.Fatalf("兑换请求体不是合法 JSON: %v", err)
	}
	if got := payload["RefreshToken"]; got != traeRealRefreshToken {
		t.Fatalf("应优先取 refreshToken，实际传给兑换的是 %v", got)
	}
}

// TestTraeParseCallbackErrors 锁住三条用户可见报错文案（逐字，会显示在面板上）。
func TestTraeParseCallbackErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"空串", "", "没有收到内容，请把浏览器地址栏里的完整链接粘贴进来"},
		{"只有空白", "   ", "没有收到内容，请把浏览器地址栏里的完整链接粘贴进来"},
		{"授权页链接", TraeBuildAuthURL("t", "m", "1", ""), "这是 Trae 授权页的链接，不是授权完成后的回调链接。请先在 Trae 页面完成登录，然后复制跳转到的 127.0.0.1 那条地址。"},
		{"中文说明", "这不是一个链接", "无法从粘贴内容里解析出授权参数，请确认复制的是完整链接"},
		{"人的句子", "我登录好了，你看看", "无法从粘贴内容里解析出授权参数，请确认复制的是完整链接"},
		{"有查询串但无凭据", "http://127.0.0.1:8765/authorize?foo=bar", "无法从粘贴内容里解析出授权参数，请确认复制的是完整链接"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TraeParseCallback(tc.raw)
			if err == nil {
				t.Fatalf("输入 %q 应报错", tc.raw)
			}
			if err.Error() != tc.want {
				t.Fatalf("报错文案不符\n got %q\nwant %q", err.Error(), tc.want)
			}
		})
	}
}

// TestTraeParseCallbackAuthorizationLinkKeepsBothKeywords 授权页链接的报错里要同时
// 点出「授权页」和「127.0.0.1」，用户才知道下一步该复制哪条地址。
func TestTraeParseCallbackAuthorizationLinkKeepsBothKeywords(t *testing.T) {
	_, err := TraeParseCallback(TraeBuildAuthURL("t", "m", "1", ""))
	if err == nil {
		t.Fatal("授权页链接应被拒绝")
	}
	if !strings.Contains(err.Error(), "授权页") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("报错文案缺少关键提示: %q", err.Error())
	}
}

// TestTraeParseCallbackNoTokenAfterExchange 有凭据字段但既没 token 也没 refreshToken：
// 报「没有有效的 userJwt」，而不是写出一个空账号。
//
// 用 `?code=` 触发：它属于凭据字段（能通过「这是回调参数」判定），但不在 refresh 的
// 回退链里，所以不会触发兑换，最终因缺 token 走到这条报错。
func TestTraeParseCallbackNoTokenAfterExchange(t *testing.T) {
	_, err := TraeParseCallback("http://127.0.0.1:8765/authorize?code=abc123")
	if err == nil {
		t.Fatal("有凭据字段但没有 token 时应报错")
	}
	const want = "粘贴的内容里没有有效的 userJwt，请确认授权已完成"
	if err.Error() != want {
		t.Fatalf("报错文案不符\n got %q\nwant %q", err.Error(), want)
	}
}

// TestTraeParseCallbackPropagatesExchangeError 兑换失败的原因要原样透出（面板要能
// 显示「refreshToken 可能已失效」这类可操作信息），不能被改写成别的文案。
func TestTraeParseCallbackPropagatesExchangeError(t *testing.T) {
	traeStubExchange(t, 200, `{"Result":{"Token":"","UserInfo":""}}`)
	_, err := TraeParseCallback("http://127.0.0.1:8765/authorize?refreshToken=legacy-refresh")
	if err == nil {
		t.Fatal("兑换未返回 Token 时应报错")
	}
	if !strings.Contains(err.Error(), "ExchangeToken 未返回 Token（refreshToken 可能已失效）") {
		t.Fatalf("兑换报错应原样透出，实际: %v", err)
	}
}

// TestTraeNormalizeExpire 秒 / 毫秒 / 空 / 垃圾值。
func TestTraeNormalizeExpire(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want int64
	}{
		{"秒", "1791700000", 1791700000},
		{"毫秒", "1791700000000", 1791700000},
		{"空串", "", 0},
		{"nil", nil, 0},
		{"非数字", "not-a-number", 0},
		{"带空白", "  1791700000  ", 1791700000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraeNormalizeExpire(tc.raw); got != tc.want {
				t.Fatalf("TraeNormalizeExpire(%v) = %d，想要 %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTraeBuildAuthURLParameters 授权链接必须带齐全部参数，且值逐字正确。
func TestTraeBuildAuthURLParameters(t *testing.T) {
	raw := TraeBuildAuthURL("trace-1", "machine-1", "1234567890123456789", "")
	if !strings.HasPrefix(raw, TraeAuthURL+"?") {
		t.Fatalf("授权链接前缀不对: %s", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("授权链接无法解析: %v", err)
	}
	query := parsed.Query()
	want := map[string]string{
		"login_version":     "1",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"plugin_version":    "2.3.24254",
		"auth_type":         "local",
		"client_id":         "ono9krqynydwx5",
		"redirect":          "0",
		"login_trace_id":    "trace-1",
		"auth_callback_url": "http://127.0.0.1:8765/authorize",
		"machine_id":        "machine-1",
		"device_id":         "1234567890123456789",
		"x_device_id":       "1234567890123456789",
		"x_machine_id":      "machine-1",
		"x_device_brand":    "Mac14,7",
		"x_device_type":     "mac",
		"x_os_version":      "macOS 26.4.1",
		"x_env":             "",
		"x_app_version":     "0.1.7",
		"x_app_type":        "stable",
		"hide_saas_login":   "true",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("参数 %s = %q，想要 %q", key, got, value)
		}
	}
	if len(query) != len(want) {
		t.Errorf("参数个数 = %d，想要 %d（多了或少了参数）", len(query), len(want))
	}
	// x_env 是空值参数，url.ParseQuery 默认会保留它（存在且为空串）。
	if _, ok := query["x_env"]; !ok {
		t.Error("x_env 必须出现（哪怕值为空）")
	}
	// 手工拼出的编码形态也要与 Python 的 urlencode 一致。
	if !strings.Contains(raw, "auth_callback_url=http%3A%2F%2F127.0.0.1%3A8765%2Fauthorize") {
		t.Errorf("auth_callback_url 的编码形态不对: %s", raw)
	}
	if !strings.Contains(raw, "client_id=ono9krqynydwx5") {
		t.Errorf("缺少 client_id: %s", raw)
	}
}

// TestTraeBuildAuthURLCustomCallback 传入公网回调地址时必须原样使用。
func TestTraeBuildAuthURLCustomCallback(t *testing.T) {
	raw := TraeBuildAuthURL("t", "m", "1", "https://example.com/admin/trae/callback")
	want := "auth_callback_url=https%3A%2F%2Fexample.com%2Fadmin%2Ftrae%2Fcallback"
	if !strings.Contains(raw, want) {
		t.Fatalf("自定义回调未被使用: %s", raw)
	}
}

// TestTraeExchangeParsesNestedJSONString 兑换返回里的 UserInfo 本身是 JSON 字符串，
// 必须二次解析（否则 uid/nickname 全空，账号写不出来）。
func TestTraeExchangeParsesNestedJSONString(t *testing.T) {
	body := `{"Result":{"Token":"tok","RefreshToken":"rt","TokenExpireAt":"1791700000",` +
		`"ClientID":"ono9krqynydwx5","UserInfo":"{\"UserID\":\"9\",\"ScreenName\":\"nick\",\"Region\":\"CN\"}"}}`
	traeStubExchange(t, 200, body)

	out, err := TraeExchangeRefreshToken(context.Background(), "legacy-refresh", "", "")
	if err != nil {
		t.Fatalf("兑换应成功: %v", err)
	}
	if out["token"] != "tok" {
		t.Fatalf("token = %v", out["token"])
	}
	if out["refreshToken"] != "rt" {
		t.Fatalf("refreshToken = %v", out["refreshToken"])
	}
	if out["tokenExpireAt"] != "1791700000" {
		t.Fatalf("tokenExpireAt = %v（数字被解成 float64 会变形）", out["tokenExpireAt"])
	}
	userInfo, ok := out["userInfo"].(map[string]any)
	if !ok {
		t.Fatalf("userInfo 不是对象: %#v", out["userInfo"])
	}
	if userInfo["UserID"] != "9" || userInfo["ScreenName"] != "nick" {
		t.Fatalf("嵌套 UserInfo 解析错误: %#v", userInfo)
	}
	if out["host"] != TraeExchangeHost {
		t.Fatalf("host = %v", out["host"])
	}
}

// TestTraeExchangeAcceptsTopLevelBody 返回体本身就是结果（没有 Result 包裹）时也要能用。
func TestTraeExchangeAcceptsTopLevelBody(t *testing.T) {
	traeStubExchange(t, 200, `{"Token":"tok","UserInfo":"{\"UserID\":\"7\"}"}`)
	out, err := TraeExchangeRefreshToken(context.Background(), "rt", "", "")
	if err != nil {
		t.Fatalf("顶层结果应解析成功: %v", err)
	}
	if out["token"] != "tok" {
		t.Fatalf("token = %v", out["token"])
	}
	if out["refreshToken"] != "rt" {
		t.Fatalf("缺 RefreshToken 时应回落到原始 refreshToken，实际 %v", out["refreshToken"])
	}
}

// TestTraeExchangeErrorMessages 兑换失败的三类报错文案。
func TestTraeExchangeErrorMessages(t *testing.T) {
	t.Run("HTTP 错误", func(t *testing.T) {
		traeStubExchange(t, 500, "boom")
		_, err := TraeExchangeRefreshToken(context.Background(), "rt", "", "")
		if err == nil || !strings.Contains(err.Error(), "ExchangeToken HTTP 500: boom") {
			t.Fatalf("报错不符: %v", err)
		}
	})
	t.Run("缺 Token", func(t *testing.T) {
		traeStubExchange(t, 200, `{"Result":{"Token":""}}`)
		_, err := TraeExchangeRefreshToken(context.Background(), "rt", "", "")
		if err == nil || !strings.Contains(err.Error(), "ExchangeToken 未返回 Token（refreshToken 可能已失效）") {
			t.Fatalf("报错不符: %v", err)
		}
	})
	t.Run("结构异常", func(t *testing.T) {
		traeStubExchange(t, 200, `{"Result":"not-an-object"}`)
		_, err := TraeExchangeRefreshToken(context.Background(), "rt", "", "")
		if err == nil || !strings.Contains(err.Error(), "ExchangeToken 返回结构异常") {
			t.Fatalf("报错不符: %v", err)
		}
	})
	t.Run("空 refreshToken 直接返回空", func(t *testing.T) {
		out, err := TraeExchangeRefreshToken(context.Background(), "", "", "")
		if err != nil {
			t.Fatalf("空 refreshToken 不应报错: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("空 refreshToken 应返回空 map，实际 %#v", out)
		}
	})
}

// TestTraeJSONLoadsSafe 对应 Python 的 _json_loads_safe：空/非法/非对象一律 {}。
func TestTraeJSONLoadsSafe(t *testing.T) {
	if got := traeJSONLoadsSafe(""); len(got) != 0 {
		t.Fatalf("空串应返回空 map，实际 %#v", got)
	}
	if got := traeJSONLoadsSafe("not json"); len(got) != 0 {
		t.Fatalf("非法 JSON 应返回空 map，实际 %#v", got)
	}
	if got := traeJSONLoadsSafe(`[1,2,3]`); len(got) != 0 {
		t.Fatalf("非对象应返回空 map，实际 %#v", got)
	}
	if got := traeJSONLoadsSafe(`{"a":1}`); got["a"] == nil {
		t.Fatalf("合法对象应被解析: %#v", got)
	}
}

// TestTraeLooksLikeBareToken 裸 token 形状检查的边界。
func TestTraeLooksLikeBareToken(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"真实 token", traeRealRefreshToken, true},
		{"太短", "abcdefg", false},
		{"含空格", "abcdefghij klmnopqrst", false},
		{"含中文", "我登录好了你看看呀呀呀呀呀", false},
		{"恰好 16 位", "abcdefghijklmnop", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := traeLooksLikeBareToken(tc.text); got != tc.want {
				t.Fatalf("traeLooksLikeBareToken(%q) = %v，想要 %v", tc.text, got, tc.want)
			}
		})
	}
}

// assertTraeCred 断言凭据字典里某个键的值（字符串比较，避免类型歧义）。
func assertTraeCred(t *testing.T, creds map[string]any, key, want string) {
	t.Helper()
	got, ok := creds[key]
	if !ok {
		t.Fatalf("凭据缺少键 %s", key)
	}
	if text, _ := got.(string); text != want {
		t.Fatalf("凭据 %s = %#v，想要 %q", key, got, want)
	}
}
