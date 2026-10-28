// ═══ 更新日志 ═══
// 2026-10-28：新增。锁住 ExchangeToken 响应里时间字段的字符串/数字双形态。
package trae

import (
	"encoding/json"
	"testing"
)

// 2026-10-28 实测：线上 6 个 Trae 号的 token 保活连续两周每天全失败，
// 报 `json: cannot unmarshal number into Go struct field .Result.RefreshExpireAt
// of type string`。原因是上游把 RefreshExpireAt 返回成数字（毫秒时间戳），
// 而结构体声明成 string，导致**整个响应解析失败**——token 其实刷新成功了，
// 却被丢弃，账号一路过期到 1.6~2.7 天前，面板显示「凭证已过期 / 查询失败」。
func TestExchangeResponseAcceptsNumericExpiry(t *testing.T) {
	// 线上真实响应片段（RefreshExpireAt 是数字）
	body := `{"Result":{"ClientID":"ono9krqynydwx5","RefreshExpireAt":1807365600661,` +
		`"RefreshToken":"eIIOsW--ngiZB6bZMLb6sJ1Ji1NKpImKcIx8oQcz81E=.18ddccf9f316e199",` +
		`"Token":"eyJhbGciOiJSUzI1NiJ9.payload.sig"}}`
	var out exchangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("数字形态的 RefreshExpireAt 应能解析，实际报错：%v", err)
	}
	if out.Result.Token == "" {
		t.Fatal("Token 应被解析出来")
	}
	if out.Result.RefreshToken == "" {
		t.Fatal("RefreshToken 应被解析出来")
	}
	if got := string(out.Result.RefreshExpireAt); got != "1807365600661" {
		t.Errorf("RefreshExpireAt=%q want 1807365600661", got)
	}
}

// 字符串形态必须继续支持：上游不同版本两种都出现过。
func TestExchangeResponseAcceptsStringExpiry(t *testing.T) {
	body := `{"Result":{"Token":"jwt","RefreshToken":"rt",` +
		`"TokenExpireAt":"1807365600","RefreshExpireAt":"1807365600661"}}`
	var out exchangeResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("字符串形态应能解析：%v", err)
	}
	if got := parseExpire(string(out.Result.TokenExpireAt)); got != 1807365600 {
		t.Errorf("TokenExpireAt 解析=%d want 1807365600", got)
	}
}

// 缺字段 / null 不该让整条响应失败（RefreshExpireAt 不是刷新成功与否的判据）。
func TestExchangeResponseToleratesMissingExpiry(t *testing.T) {
	for _, body := range []string{
		`{"Result":{"Token":"jwt","RefreshToken":"rt"}}`,
		`{"Result":{"Token":"jwt","RefreshToken":"rt","RefreshExpireAt":null}}`,
		`{"Result":{"Token":"jwt","RefreshToken":"rt","RefreshExpireAt":""}}`,
	} {
		var out exchangeResponse
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Errorf("应容忍缺失/空的 RefreshExpireAt，body=%s err=%v", body, err)
		}
		if out.Result.Token != "jwt" {
			t.Errorf("Token 应被解析，body=%s", body)
		}
	}
}
