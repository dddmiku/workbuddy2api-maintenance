// ═══ 更新日志 ═══
// 2026-10-12：初版——邀请奖励（code fission）发奖调用的错误码语义与请求形态。
package trae

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// fissionStub 用假上游替换 TraeAPIHost，返回一个已解码的请求记录。
//
// 覆盖的是完整链路（postTraeAPI 的 URL 拼接、头族、body 序列化 + 响应判定），
// 而不只是判定函数——「端点拼错」和「头漏了」这类问题只有走真链路才抓得到。
func fissionStub(t *testing.T, status int, body string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.Path = r.URL.Path
		cap.Method = r.Method
		cap.Header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		cap.Body = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	old := TraeAPIHost
	TraeAPIHost = srv.URL
	t.Cleanup(func() {
		TraeAPIHost = old
		srv.Close()
	})
	return srv, cap
}

type capturedRequest struct {
	Path   string
	Method string
	Header http.Header
	Body   []byte
}

func testAuth() *auth.Auth {
	a := &auth.Auth{}
	a.UID = "2081842786229369"
	a.AccessToken = "test-token"
	return a
}

// TestGrantFissionRequestShape 发奖请求必须是 POST 到 grant 端点、
// body 恰好一个 activity_id 字段、头族齐全。
//
// 2026-10-12 实测：真调用返回 {"code":0,"message":"success"}，积分 4500→5000。
// 这里锁的是「调用形态」，形态错了上游会给 4001 参数无效（share_link 实测如此）。
func TestGrantFissionRequestShape(t *testing.T) {
	_, cap := fissionStub(t, 200, `{"code":0,"message":"success"}`)

	res, err := GrantCodeFission(context.Background(), testAuth(), 0)
	if err != nil {
		t.Fatalf("GrantCodeFission: %v", err)
	}
	if !res.Granted {
		t.Fatalf("应判定为已发放，实际 %+v", res)
	}
	if cap.Method != http.MethodPost {
		t.Errorf("方法应为 POST，实际 %s", cap.Method)
	}
	if cap.Path != "/trae/api/v2/ug/code_fission/grant" {
		t.Errorf("端点路径不对：%s", cap.Path)
	}
	// body 恰好一个字段，且值等于活动号。
	var body map[string]any
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("body 不是 JSON：%s", cap.Body)
	}
	if len(body) != 1 || body["activity_id"] != FissionActivityID {
		t.Fatalf("body 形态不对：%s", cap.Body)
	}
	// 头族：客户端发奖走的就是这一套（Cloud-IDE-JWT + x-device-*）。
	if got := cap.Header.Get("Authorization"); got != "Cloud-IDE-JWT test-token" {
		t.Errorf("鉴权头不对：%q", got)
	}
	for _, key := range []string{"x-device-id", "x-device-brand", "x-device-type"} {
		if cap.Header.Get(key) == "" {
			t.Errorf("缺少 %s", key)
		}
	}
	if cap.Header.Get("x-device-id") != checkinDeviceIDGen("2081842786229369", 0) {
		t.Errorf("gen=0 设备号应为默认派生值，实际 %s", cap.Header.Get("x-device-id"))
	}
}

// TestGrantFissionDeviceGenChangesHeader gen 必须真的改变出站设备号。
//
// 「同一台电脑只能被邀请一次」在上游就是靠 x-device-id 判的。若 gen 不影响
// 出站头，补发工具换 gen 重试就是原地重打，永远绕不过判重。
func TestGrantFissionDeviceGenChangesHeader(t *testing.T) {
	_, cap := fissionStub(t, 200, `{"code":0,"message":"success"}`)
	if _, err := GrantCodeFission(context.Background(), testAuth(), 7); err != nil {
		t.Fatalf("GrantCodeFission: %v", err)
	}
	want := checkinDeviceIDGen("2081842786229369", 7)
	if cap.Header.Get("x-device-id") != want {
		t.Fatalf("gen=7 设备号应为 %s，实际 %s", want, cap.Header.Get("x-device-id"))
	}
	if want == checkinDeviceIDGen("2081842786229369", 0) {
		t.Fatal("gen=7 不应等于 gen=0")
	}
}

// TestGrantFissionResponseSemantics 覆盖各类上游响应的判定分支。
//
// 响应形态取自客户端实现（先看 HTTP status 再看 code，code 非 0 即失败）
// 与 2026-10-12 实测。9091 的判重语义尤其关键：判错会让可补发的奖励被误报失败。
func TestGrantFissionResponseSemantics(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantGranted bool
		wantAlready bool
		wantErr     bool
		wantDevice  bool
	}{
		{name: "success", status: 200, body: `{"code":0,"message":"success"}`, wantGranted: true},
		{name: "already granted", status: 200, body: `{"code":1028,"message":"已发放"}`, wantAlready: true},
		{name: "not invited", status: 200, body: `{"code":1027,"message":"未被邀请"}`, wantErr: true},
		{name: "no benefit", status: 200, body: `{"code":9070,"message":"暂无可领取权益"}`, wantErr: true},
		{name: "activity unavailable", status: 200, body: `{"code":9090,"message":"活动暂不可用"}`, wantErr: true},
		{name: "device used", status: 200, body: `{"code":9091,"message":"该设备已参与"}`, wantErr: true, wantDevice: true},
		{name: "unknown code", status: 200, body: `{"code":555,"message":"??"}`, wantErr: true},
		{name: "missing code", status: 200, body: `{"message":"no code field"}`, wantErr: true},
		{name: "http error", status: 500, body: `{"code":0}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fissionStub(t, tc.status, tc.body)
			res, err := GrantCodeFission(context.Background(), testAuth(), 0)
			if tc.wantErr && err == nil {
				t.Fatalf("期望错误，实际 nil（res=%+v）", res)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望成功，实际 %v", err)
			}
			if res.Granted != tc.wantGranted {
				t.Errorf("Granted=%v want %v", res.Granted, tc.wantGranted)
			}
			if res.Already != tc.wantAlready {
				t.Errorf("Already=%v want %v", res.Already, tc.wantAlready)
			}
			if tc.wantDevice && !IsFissionDeviceUsed(err) {
				t.Errorf("9091 应是 FissionDeviceUsedError，实际 %T: %v", err, err)
			}
		})
	}
}

// TestFissionDeviceUsedErrorIsTyped 9091（设备判重）必须是可判定的具名错误。
//
// 与签到的 9074 同构：上游 message 是中文文案会漂移，靠字符串匹配是脆的。
// 调用方据此决定「换 gen 重试一次」——判错会让可补发的奖励被误报成失败。
func TestFissionDeviceUsedErrorIsTyped(t *testing.T) {
	err := error(&FissionDeviceUsedError{Message: "该设备已参与过活动"})
	if !IsFissionDeviceUsed(err) {
		t.Fatal("9091 应被 IsFissionDeviceUsed 识别")
	}
	// 包装后仍可识别（errors.As 语义）。
	wrapped := errors.Join(errors.New("outer"), err)
	if !IsFissionDeviceUsed(wrapped) {
		t.Fatal("包装后仍应可识别")
	}
	// 其它错误不得误判。
	if IsFissionDeviceUsed(errors.New("trae fission grant: code 1027: 无邀请记录")) {
		t.Fatal("非 9091 不该被识别为设备判重")
	}
	if IsFissionDeviceUsed(nil) {
		t.Fatal("nil 不该被识别为设备判重")
	}
	if got := err.Error(); !strings.Contains(got, "9091") {
		t.Fatalf("错误文案应含码值，实际 %q", got)
	}
}

// TestFissionCodesAreDistinct 四个业务码必须互不相同，且与签到限流码 9074 不冲突。
//
// 码值撞车会让「设备判重」被当成「无邀请记录」处理，静默丢掉可补发的奖励。
func TestFissionCodesAreDistinct(t *testing.T) {
	codes := map[float64]string{
		FissionCodeNotInvited:          "NotInvited",
		FissionCodeAlreadyGranted:      "AlreadyGranted",
		FissionCodeActivityUnavailable: "ActivityUnavailable",
		FissionCodeDeviceUsed:          "DeviceUsed",
		FissionCodeNoBenefit:           "NoBenefit",
	}
	if len(codes) != 5 {
		t.Fatalf("业务码有重复：%v", codes)
	}
	if _, clash := codes[RateLimitCode]; clash {
		t.Fatal("fission 业务码不得与签到限流码 9074 撞车")
	}
}

// TestFissionActivityIDMatchesClient 活动号必须与客户端硬编码的一致。
//
// 客户端在 handleCommercialActivity 里用 `e==="code_fission_202609"` 分流到
// 专用 grant 端点；活动号写错会落到通用 activity/action 分支，得不到积分。
func TestFissionActivityIDMatchesClient(t *testing.T) {
	if FissionActivityID != "code_fission_202609" {
		t.Fatalf("活动号必须与 TRAE 客户端一致，实际 %q", FissionActivityID)
	}
	if fissionGrantPath != "/trae/api/v2/ug/code_fission/grant" {
		t.Fatalf("grant 端点路径不对：%q", fissionGrantPath)
	}
}

// TestFissionShareLinkParsesCode share_link 必须把 invite_code 取出来。
//
// 邀请码用于核对「哪条邀请记录属于哪个账号」：记录里的邀请方 ↔ 这里的码。
// 2026-10-12 实测 uid 2081842786229369 → "Z2EAVV5FUBPT"。
func TestFissionShareLinkParsesCode(t *testing.T) {
	_, cap := fissionStub(t, 200, `{"code":0,"invite_code":"Z2EAVV5FUBPT","message":"success"}`)
	code, err := FissionShareLink(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("FissionShareLink: %v", err)
	}
	if code != "Z2EAVV5FUBPT" {
		t.Fatalf("邀请码解析错：%q", code)
	}
	if cap.Path != "/trae/api/v2/ug/code_fission/share_link" {
		t.Errorf("端点路径不对：%s", cap.Path)
	}
	// 非 0 码必须报错，不能把空码当成功返回。
	fissionStub(t, 200, `{"code":4001,"invite_code":"","message":"param is invalid"}`)
	if _, err := FissionShareLink(context.Background(), testAuth()); err == nil {
		t.Fatal("code 4001 应报错")
	}
}
