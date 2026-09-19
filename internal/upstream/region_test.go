package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// regionStub 模拟 global 属性域三个端点：register / area / login-account。
// 用 registerNeedsRegion 控制账号「已登记 / 缺区域」两种形态。
type regionStub struct {
	registerNeedsRegion bool
	registerHits        int
	areaHits            int
	loginAccountHits    int
	lastLoginBody       map[string]any
	areaJSON            string
}

func (s *regionStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case registerPath:
			s.registerHits++
			if s.registerNeedsRegion && s.loginAccountHits == 0 {
				// 缺区域：HTTP 200 + 业务 code 500（与上游实测一致）。
				w.Write([]byte(`{"code":500,"msg":"register failed:register region required"}`))
				return
			}
			w.Write([]byte(`{"code":200,"msg":"register success"}`))
		case userAreaInfoPath:
			s.areaHits++
			w.Write([]byte(s.areaJSON))
		case loginAccountPath:
			s.loginAccountHits++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.lastLoginBody = body
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"code":404,"msg":"not found"}`))
		}
	})
}

func newRegionStub(t *testing.T, needsRegion bool) (*regionStub, *Client, *auth.Auth) {
	t.Helper()
	area := `{"code":0,"msg":"OK","data":"{\"code\":0,\"msg\":\"ok\",\"data\":{\"country\":\"新加坡\",\"IOS2\":\"SG\",\"IOS3\":\"SGP\",\"enName\":\"Singapore\",\"name\":\"新加坡\",\"code\":\"65\"}}"}`
	stub := &regionStub{registerNeedsRegion: needsRegion, areaJSON: area}
	server := httptest.NewServer(stub.handler())
	t.Cleanup(server.Close)
	auth.SetGlobalEnabled(true)
	client := &Client{
		HTTP:          server.Client(),
		ChatHTTP:      server.Client(),
		GlobalEnabled: true,
		ChatBaseCN:    server.URL,
		BillingBaseCN: server.URL,
		// global 属性域 base 也要指向桩，否则会打到真实 www.workbuddy.ai。
		ChatBaseGlobal:    server.URL,
		BillingBaseGlobal: server.URL,
	}
	acct := &auth.Auth{UID: "u1", AccessToken: "tok", Domain: "www.workbuddy.ai"}
	return stub, client, acct
}

// 缺区域：先调 register 拿 region required → 探测区域 → 写账号属性 → 再 register 成功。
func TestCompleteRegionRepairsMissingRegion(t *testing.T) {
	stub, client, acct := newRegionStub(t, true)
	area, err := client.CompleteRegion(acct)
	if err != nil {
		t.Fatalf("CompleteRegion: %v", err)
	}
	if area.IOS2 != "SG" || area.Code != "65" || area.EnName != "Singapore" {
		t.Errorf("area=%+v want SG/65/Singapore", area)
	}
	if stub.loginAccountHits != 1 {
		t.Errorf("login-account hits=%d want 1", stub.loginAccountHits)
	}
	if stub.registerHits != 2 {
		t.Errorf("register hits=%d want 2 (probe + re-activate)", stub.registerHits)
	}
	attrs, _ := stub.lastLoginBody["attributes"].(map[string]any)
	if attrs == nil {
		t.Fatalf("attributes missing in %v", stub.lastLoginBody)
	}
	for field, want := range map[string]string{"countryCode": "65", "countryFullName": "Singapore", "countryName": "SG"} {
		got, _ := attrs[field].([]any)
		if len(got) != 1 || got[0] != want {
			t.Errorf("attributes[%s]=%v want [%s]", field, attrs[field], want)
		}
	}
}

// 已登记：register 直接成功 → 不改账号属性（保持幂等、无多余写请求）。
func TestCompleteRegionSkipsWhenAlreadyRegistered(t *testing.T) {
	stub, client, acct := newRegionStub(t, false)
	if _, err := client.CompleteRegion(acct); err != nil {
		t.Fatalf("CompleteRegion: %v", err)
	}
	if stub.registerHits != 1 {
		t.Errorf("register hits=%d want 1", stub.registerHits)
	}
	if stub.loginAccountHits != 0 {
		t.Errorf("login-account hits=%d want 0 (nothing to repair)", stub.loginAccountHits)
	}
	if stub.areaHits != 0 {
		t.Errorf("area hits=%d want 0 (nothing to repair)", stub.areaHits)
	}
}

// CN 账号不适用：region 是国际版专有问题，直接报错且不打上游。
func TestCompleteRegionRejectsCNAccount(t *testing.T) {
	stub, client, _ := newRegionStub(t, true)
	cn := &auth.Auth{UID: "cn1", AccessToken: "tok", Domain: "www.codebuddy.cn"}
	if _, err := client.CompleteRegion(cn); err == nil {
		t.Fatal("CN account must be rejected")
	}
	if stub.registerHits != 0 || stub.loginAccountHits != 0 {
		t.Error("CN account must not hit global endpoints")
	}
}

// RegionRequired 判定：只认 14017 + trial 文案，其它错误不误判。
func TestRegionRequiredClassification(t *testing.T) {
	const msg = `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated."}}}`
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"429 trial", 429, msg, true},
		{"403 trial", 403, msg, true},
		{"400 trial", 400, msg, true},
		{"429 11140 banned", 429, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false},
		{"429 rate limit only", 429, `{"code":6004,"msg":"rate limited"}`, false},
		{"500 trial", 500, msg, false},
		{"200 ok", 200, msg, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RegionRequired(tc.status, tc.body); got != tc.want {
				t.Errorf("RegionRequired(%d)=%v want %v", tc.status, got, tc.want)
			}
		})
	}
}

// 上游返回对象形态（非 JSON 字符串）时也能解析出区域。
func TestUserAreaAcceptsObjectPayload(t *testing.T) {
	area := `{"code":0,"msg":"OK","data":{"code":0,"msg":"ok","data":{"IOS2":"US","IOS3":"USA","enName":"United States","name":"美国","code":"840"}}}`
	stub := &regionStub{areaJSON: area}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	client := &Client{HTTP: server.Client(), GlobalEnabled: true,
		ChatBaseGlobal: server.URL, BillingBaseGlobal: server.URL}
	acct := &auth.Auth{UID: "u1", AccessToken: "tok", Domain: "www.workbuddy.ai"}
	got, err := client.UserArea(acct)
	if err != nil {
		t.Fatalf("UserArea: %v", err)
	}
	if got.IOS2 != "US" || got.Code != "840" {
		t.Errorf("area=%+v want US/840", got)
	}
}

// 区域信息不完整时按错误处理，不写入半截属性。
func TestUserAreaRejectsIncompletePayload(t *testing.T) {
	stub := &regionStub{areaJSON: `{"code":0,"msg":"OK","data":"{\"code\":0,\"data\":{\"IOS2\":\"SG\"}}"}`}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	client := &Client{HTTP: server.Client(), GlobalEnabled: true,
		ChatBaseGlobal: server.URL, BillingBaseGlobal: server.URL}
	acct := &auth.Auth{UID: "u1", AccessToken: "tok", Domain: "www.workbuddy.ai"}
	if _, err := client.UserArea(acct); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err=%v want incomplete-area error", err)
	}
}
