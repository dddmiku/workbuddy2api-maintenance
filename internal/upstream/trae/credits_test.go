// ═══ 更新日志 ═══
// 2026-10-15：初版——签到链路（状态/领取）的业务码语义、字段校验与错误分类。
//
// 为什么这批用例必须存在：签到原先在 trae 包里**零测试覆盖**（credits.go 没有
// 任何测试文件），而它恰好是线上长期出问题的地方——「9074 换设备号后当天不再
// 重试」这个错误结论就是这么漏出去的。本文件把实测结论固化成回归。
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

// checkinStub 用假上游替换 TraeAPIHost，按路径返回不同响应。
//
// 走完整链路（postTraeAPI 的 URL 拼接、头族、body 序列化 + 响应判定），而不只是
// 判定函数——「端点拼错」「头漏了」这类问题只有走真链路才抓得到。
type checkinStub struct {
	statusBody string
	claimBody  string
	statusCode int
	claimCode  int
	calls      []string
	headers    []http.Header
	bodies     [][]byte
}

func (s *checkinStub) server(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, r.URL.Path)
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, raw)
		code, body := s.statusCode, s.statusBody
		if strings.HasSuffix(r.URL.Path, "/claim") {
			code, body = s.claimCode, s.claimBody
		}
		if code == 0 {
			code = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	old := TraeAPIHost
	TraeAPIHost = srv.URL
	t.Cleanup(func() {
		TraeAPIHost = old
		srv.Close()
	})
}

// TestCheckinRequestShape 签到请求的端点、方法、body 与头族。
//
// x-device-id 是签到的去重键（上游按设备维度判「今天签过没」），必须存在且是
// 纯数字形态；body 恒为 `{}`（上游不接受额外字段）。
func TestCheckinRequestShape(t *testing.T) {
	stub := &checkinStub{
		statusBody: `{"code":0,"enable":true,"checked_in":false,"credits":100}`,
		claimBody:  `{"code":0,"message":"success"}`,
	}
	stub.server(t)
	a := testAuth()

	if _, err := CheckinStatusOf(context.Background(), a); err != nil {
		t.Fatalf("查状态失败：%v", err)
	}
	if _, err := ClaimCheckin(context.Background(), a, 0); err != nil {
		t.Fatalf("领奖失败：%v", err)
	}
	want := []string{"/trae/api/v2/ug/checkin_credits/status", "/trae/api/v2/ug/checkin_credits/claim"}
	if len(stub.calls) != 2 || stub.calls[0] != want[0] || stub.calls[1] != want[1] {
		t.Fatalf("端点错误：%v，应为 %v", stub.calls, want)
	}
	for i, h := range stub.headers {
		if got := h.Get("Authorization"); got != "Cloud-IDE-JWT "+a.AccessToken {
			t.Errorf("第 %d 次请求 Authorization=%q，应为 Cloud-IDE-JWT 前缀", i, got)
		}
		if got := h.Get("x-device-id"); got == "" {
			t.Errorf("第 %d 次请求缺 x-device-id（上游按设备维度去重）", i)
		} else if strings.Trim(got, "0123456789") != "" {
			t.Errorf("第 %d 次请求 x-device-id=%q，应为纯数字形态", i, got)
		}
		if got := h.Get("x-device-brand"); got == "" {
			t.Errorf("第 %d 次请求缺 x-device-brand", i)
		}
		if got := h.Get("x-device-type"); got != "windows" {
			t.Errorf("第 %d 次请求 x-device-type=%q，应为 windows", i, got)
		}
	}
	for i, b := range stub.bodies {
		if strings.TrimSpace(string(b)) != "{}" {
			t.Errorf("第 %d 次请求 body=%q，应为 {}", i, string(b))
		}
	}
}

// TestCheckinDeviceIDGeneration 设备号代数：gen=0 与历史一致，gen>0 必须是**另一个**号。
//
// 9074 是设备维度限流，换一个**新派生**的设备号即可立刻签上——所以各代之间
// 必须互不相同，否则「换号重试」等于原地重发（实测 2026-10-11：同一账号同一 JWT，
// 第 0 代 9074、第 1 代立刻 success）。
func TestCheckinDeviceIDGeneration(t *testing.T) {
	a := testAuth()
	gen0 := checkinDeviceIDGen(a.UID, 0)
	if gen0 != checkinDeviceID(a.UID) {
		t.Fatal("gen=0 应与 checkinDeviceID 同值（保持历史设备号不变）")
	}
	seen := map[string]int{gen0: 0}
	for gen := 1; gen <= 4; gen++ {
		got := checkinDeviceIDGen(a.UID, gen)
		if got == "" {
			t.Fatalf("gen=%d 派生为空", gen)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("gen=%d 与 gen=%d 派生相同（%s）——换号重试会原地重发", gen, prev, got)
		}
		seen[got] = gen
		if len(got) != 16 || strings.Trim(got, "0123456789") != "" {
			t.Fatalf("gen=%d 设备号 %q 不是 16 位纯数字", gen, got)
		}
	}
	// 跨账号不能碰撞（否则一个账号的 9074 会牵连另一个）。
	other := &auth.Auth{UID: "181886336895657"}
	if checkinDeviceIDGen(other.UID, 0) == gen0 {
		t.Fatal("不同账号的第 0 代设备号不应相同")
	}
}

// TestClaimIdempotentCodeZero 已签到的账号再 claim 返回 code 0（幂等），不是 9095。
//
// 这是「自动重试循环」能成立的前提：重复调用不会浪费当天机会。2026-10-15 对
// 上游实测确认（6 个账号全部 checked_in=true 后再 claim 仍是 `{"code":0,"message":"success"}`）。
func TestClaimIdempotentCodeZero(t *testing.T) {
	stub := &checkinStub{claimBody: `{"code":0,"message":"success"}`}
	stub.server(t)
	status, err := ClaimCheckin(context.Background(), testAuth(), 0)
	if err != nil {
		t.Fatalf("已签到账号再 claim 应成功（幂等），实际 %v", err)
	}
	if !status.CheckedIn {
		t.Error("code 0 应判定为已签到")
	}
}

// TestClaimRateLimitedIsTyped 9074 必须是具名错误，且能被 errors.As 识别。
//
// 判据用类型而不是文案：上游 message 是中文，会随改版漂移，拿它做分支是脆的。
func TestClaimRateLimitedIsTyped(t *testing.T) {
	stub := &checkinStub{claimBody: `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`}
	stub.server(t)
	_, err := ClaimCheckin(context.Background(), testAuth(), 0)
	if err == nil {
		t.Fatal("9074 应报错")
	}
	if !IsRateLimited(err) {
		t.Fatalf("9074 应被 IsRateLimited 识别，实际 %v", err)
	}
	// 包装一层后仍要能识别（调用方可能 fmt.Errorf %w 过）。
	if !IsRateLimited(errors.Join(errors.New("wrap"), err)) {
		t.Fatal("包装后仍应识别 9074")
	}
}

// TestClaimDeviceAlreadyCheckedIsNotCheckedIn 9095 **绝不能**被当成「本账号已签到」。
//
// 9095 的文案是「该设备已签到」，说的是**这个设备号**今天签过——可能是别的账号
// 用同一设备号签的。参考实现 trae_client.py 的注释明确写了这一点，并有测试固化
// （`_checkin_claim_ok({"code": 9095, ...}) == False`）。误判的后果是账号当天
// 被标记为已签、再也不重试，实际一分没领到。
func TestClaimDeviceAlreadyCheckedIsNotCheckedIn(t *testing.T) {
	stub := &checkinStub{claimBody: `{"code":9095,"message":"device already checked"}`}
	stub.server(t)
	status, err := ClaimCheckin(context.Background(), testAuth(), 0)
	if err == nil {
		t.Fatal("9095 不该被当成成功")
	}
	if status.CheckedIn {
		t.Fatal("9095 绝不能判定为已签到（它是设备维度，不证明本账号签过）")
	}
	if !IsDeviceAlreadyChecked(err) {
		t.Fatalf("9095 应被 IsDeviceAlreadyChecked 识别，实际 %v", err)
	}
	if IsRateLimited(err) {
		t.Fatal("9095 不是 9074，两者不能混判")
	}
}

// TestClaimAuthFailedIsTyped 1001（服务端作废 token）必须是具名错误。
//
// 这是「停机几天后第一次签到必失败」的根因：本地 expiresAt 还没到，但服务端已不认。
// 正确处置是刷新凭据后重试，而不是当成普通失败丢掉这一轮。
func TestClaimAuthFailedIsTyped(t *testing.T) {
	stub := &checkinStub{claimBody: `{"code":1001,"message":"not able to authenticate"}`}
	stub.server(t)
	_, err := ClaimCheckin(context.Background(), testAuth(), 0)
	if !IsAuthFailure(err) {
		t.Fatalf("1001 应被 IsAuthFailure 识别，实际 %v", err)
	}
	// 文案兜底：上游偶发只给 message 不给 code。
	if !IsAuthFailure(errors.New(`trae checkin: not able to authenticate`)) {
		t.Fatal("「not able to authenticate」文案应被识别为鉴权失败")
	}
	// 宽泛判据会误伤：普通限流不该被判成鉴权失败。
	if IsAuthFailure(errors.New("trae checkin: code 9074: 当前参与用户太多")) {
		t.Fatal("普通限流不该被判为鉴权失败（会触发无谓的凭据刷新）")
	}
}

// TestStatusRequiresBooleanFields enable/checked_in 必须是布尔，否则报错而不是静默取 false。
//
// 静默取 false 的后果：「签到开着」被误判成「签到关了」，任务整轮跳过而用户
// 毫无察觉。参考实现同样在 _post_checkin 里对 status 做这个校验并抛错。
func TestStatusRequiresBooleanFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"enable 是数字", `{"code":0,"enable":1,"checked_in":false}`},
		{"checked_in 是数字", `{"code":0,"enable":true,"checked_in":1}`},
		{"enable 缺失", `{"code":0,"checked_in":false}`},
		{"checked_in 缺失", `{"code":0,"enable":true}`},
		{"enable 是字符串", `{"code":0,"enable":"true","checked_in":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &checkinStub{statusBody: tc.body}
			stub.server(t)
			if _, err := CheckinStatusOf(context.Background(), testAuth()); err == nil {
				t.Fatalf("非法 status 响应应报错，实际通过了：%s", tc.body)
			}
		})
	}
}

// TestStatusCreditsNonPositiveDropped credits 非正/非有限一律视为「没有可领积分」。
//
// 上游在「今天已签」时也会把 credits 填成当日额度，直接透出会让面板显示
// 「本次可领 100」而实际什么都没领到。
func TestStatusCreditsNonPositiveDropped(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"enable":true,"checked_in":true,"credits":0}`,
		`{"code":0,"enable":true,"checked_in":true,"credits":-5}`,
		`{"code":0,"enable":true,"checked_in":true,"credits":"100"}`,
		`{"code":0,"enable":true,"checked_in":true}`,
	} {
		stub := &checkinStub{statusBody: body}
		stub.server(t)
		status, err := CheckinStatusOf(context.Background(), testAuth())
		if err != nil {
			t.Fatalf("合法响应不该报错：%s：%v", body, err)
		}
		if status.Credits != 0 {
			t.Errorf("credits 应为 0，实际 %v（响应 %s）", status.Credits, body)
		}
	}
	// 正数照常透出。
	stub := &checkinStub{statusBody: `{"code":0,"enable":true,"checked_in":false,"credits":100,"extra_credits":50}`}
	stub.server(t)
	status, err := CheckinStatusOf(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("合法响应不该报错：%v", err)
	}
	if status.Credits != 100 || status.ExtraCredits != 50 {
		t.Fatalf("credits/extra_credits 应为 100/50，实际 %v/%v", status.Credits, status.ExtraCredits)
	}
}

// TestCheckinStatusBusinessCodeIsError status 接口的非 0 业务码必须是错误。
func TestCheckinStatusBusinessCodeIsError(t *testing.T) {
	stub := &checkinStub{statusBody: `{"code":9074,"message":"当前参与用户太多"}`}
	stub.server(t)
	_, err := CheckinStatusOf(context.Background(), testAuth())
	if err == nil {
		t.Fatal("status 返回 9074 应报错")
	}
	if !IsRateLimited(err) {
		t.Fatalf("status 的 9074 也应被 IsRateLimited 识别，实际 %v", err)
	}
}

// TestCheckinHTTPErrorIsReported HTTP 非 200 要带上状态码与响应片段。
func TestCheckinHTTPErrorIsReported(t *testing.T) {
	stub := &checkinStub{statusCode: http.StatusUnauthorized, statusBody: `{"message":"unauthorized"}`}
	stub.server(t)
	_, err := CheckinStatusOf(context.Background(), testAuth())
	if err == nil {
		t.Fatal("HTTP 401 应报错")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误里应含状态码 401，实际 %v", err)
	}
}

// TestCheckinDeviceIDVariesByGenerationInRequest 出站头里的设备号必须随 gen 变化。
//
// 这是「换设备号重试」真正生效的最后一环：派生函数对了但头里没换，等于没换。
func TestCheckinDeviceIDVariesByGenerationInRequest(t *testing.T) {
	stub := &checkinStub{claimBody: `{"code":9074,"message":"限流"}`}
	stub.server(t)
	a := testAuth()
	_, _ = ClaimCheckin(context.Background(), a, 0)
	_, _ = ClaimCheckin(context.Background(), a, 1)
	if len(stub.headers) != 2 {
		t.Fatalf("应有两次请求，实际 %d", len(stub.headers))
	}
	first := stub.headers[0].Get("x-device-id")
	second := stub.headers[1].Get("x-device-id")
	if first == second {
		t.Fatalf("gen 变化后出站设备号未变（都是 %s）——换号重试会原地重发", first)
	}
	if second != checkinDeviceIDGen(a.UID, 1) {
		t.Errorf("第二次设备号=%s，应为 gen1 的 %s", second, checkinDeviceIDGen(a.UID, 1))
	}
}

// TestFetchCreditsBusinessCodeIsError 积分接口的非 0 业务码是错误，code 缺失视为成功。
//
// 上游的判据是 `code is not None and code != 0`（参考实现 trae_client.py 的
// fetch_account_credits）——**缺 code 算成功**，与签到接口的 `code != 0` 不同。
func TestFetchCreditsBusinessCodeIsError(t *testing.T) {
	stub := &checkinStub{statusBody: `{"code":4010,"message":"no entitlement"}`}
	stub.server(t)
	if _, err := FetchCredits(context.Background(), testAuth()); err == nil {
		t.Fatal("非 0 业务码应报错")
	}
	// code 缺失 + 合法 usage_summary：视为成功。
	body, _ := json.Marshal(map[string]any{
		"usage_summary": map[string]any{"total_amount": 100.0, "consumed_amount": 30.0},
	})
	stub2 := &checkinStub{statusBody: string(body)}
	stub2.server(t)
	credits, err := FetchCredits(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("缺 code 应视为成功：%v", err)
	}
	if credits.Total != 100 || credits.Consumed != 30 || credits.Remaining != 70 {
		t.Fatalf("积分解析错误：%+v", credits)
	}
}
