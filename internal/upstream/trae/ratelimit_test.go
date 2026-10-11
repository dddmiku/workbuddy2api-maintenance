// ═══ 更新日志 ═══
// 2026-10-11：9074 限流改为具名错误：调用方靠 errors.As 判定，不匹配中文文案。
package trae

import (
	"errors"
	"strings"
	"testing"
)

// TestRateLimitErrorIsTyped 9074 必须是可判定的具名错误，而不是靠字符串匹配。
//
// 上游 message 是中文文案（"当前参与用户太多，请稍后再试"），会随上游改版漂移；
// 调度器据此决定「当天不再重试」，判错会导致限流窗口被反复延长。
func TestRateLimitErrorIsTyped(t *testing.T) {
	err := error(&RateLimitError{Message: "当前参与用户太多，请稍后再试"})
	if !IsRateLimited(err) {
		t.Fatal("9074 应被 IsRateLimited 识别")
	}
	// 包装后仍可识别（errors.As 语义）。
	wrapped := errors.Join(errors.New("outer"), err)
	if !IsRateLimited(wrapped) {
		t.Fatal("包装后仍应可识别")
	}
	// 其它错误不得误判。
	if IsRateLimited(errors.New("trae checkin: code 500: boom")) {
		t.Fatal("非 9074 不该被识别为限流")
	}
	if IsRateLimited(nil) {
		t.Fatal("nil 不该被识别为限流")
	}
	// 文案里带码值，便于日志直接读。
	if got := err.Error(); !strings.Contains(got, "9074") {
		t.Fatalf("错误文案应含码值，实际 %q", got)
	}
}

// TestCheckinDeviceRotationEscapes9074 9074 是设备维度：换新派生的设备号必须
// 得到**不同**的值，否则「轮换后重试」等于原地重打。
//
// 2026-10-11 实测（生产上游）：同一账号同一 JWT，第 0 代设备号返回 9074，
// 紧接着第 1、2 代**立刻签到成功**。参考实现 autumnsentiment/Trae2api-cn 的
// rotate_checkin_device_id 也是同一做法。
func TestCheckinDeviceRotationEscapes9074(t *testing.T) {
	const uid = "1642037707026618"
	g0 := checkinDeviceID(uid)
	g1 := checkinDeviceIDGen(uid, 1)
	g2 := checkinDeviceIDGen(uid, 2)
	if g0 == g1 || g1 == g2 || g0 == g2 {
		t.Fatalf("各代设备号必须互不相同：%s / %s / %s", g0, g1, g2)
	}
	// 形态要求：16 位纯数字（对齐真实客户端）。
	for _, id := range []string{g0, g1, g2} {
		if len(id) != 16 {
			t.Fatalf("设备号应为 16 位，实际 %q（%d 位）", id, len(id))
		}
		for _, c := range id {
			if c < '0' || c > '9' {
				t.Fatalf("设备号应为纯数字，实际 %q", id)
			}
		}
	}
	// gen=0 必须与历史派生一致——改了会让所有账号在签到侧变成「新设备」，
	// 白白丢掉上游的设备维度状态。
	if g0 != checkinDeviceIDGen(uid, 0) {
		t.Fatal("gen=0 应与 checkinDeviceID 一致")
	}
	// 稳定性：同一代数多次派生必须同值（否则每次重试都是新设备，等于无限轮换）。
	if checkinDeviceIDGen(uid, 3) != checkinDeviceIDGen(uid, 3) {
		t.Fatal("同一代数必须稳定派生")
	}
	// 跨账号不碰撞（同代数）。
	if checkinDeviceIDGen("uid-a", 1) == checkinDeviceIDGen("uid-b", 1) {
		t.Fatal("不同账号的设备号不该相同")
	}
}
