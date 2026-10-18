package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// 目录探测的回退口径：该域账号全部在冷却时，PickExcludingForRealm 返回 nil，
// 但 PickAnyForRealm 仍要能取到一个未禁用的账号。
//
// 2026-10-18 实测依据：dev 上 6 个 CN 号同时处于 hard_credit 冷却，目录探测只走
// 健康口径 → fetchDynamicModels 返回 nil → /v1/models 一个 CN 模型都不公布，
// 客户端看起来像「模型全没了」；而同一账号手动拉 /v3/config 稳定拿到 37 个模型。
// 读目录是只读探测，不该由账号冷却状态决定是否公布。
func TestPickAnyForRealmFallsBackWhenAllCooling(t *testing.T) {
	p := realmPool(t)

	// 把 CN 号全部打进余额耗尽冷却（次日 04:00 才恢复）。
	p.CooldownUntilTomorrow4AM("cn1", "余额不足")
	p.CooldownUntilTomorrow4AM("cn2", "余额不足")

	if got := p.PickExcludingForRealm(nil, "", "cn"); got != nil {
		t.Fatalf("健康口径应取不到冷却中的 CN 号，got uid=%s", got.UID)
	}

	got := p.PickAnyForRealm("cn")
	if got == nil {
		t.Fatal("PickAnyForRealm 应在健康号全冷却时仍返回该域的未禁用账号")
	}
	if realmOf(got) != "cn" {
		t.Fatalf("应返回 CN 号，got uid=%s realm=%s", got.UID, realmOf(got))
	}
}

// 域过滤不能退化成「随便哪个域都行」：CN 目录探测不能拿 global 号去拉。
func TestPickAnyForRealmRespectsRealm(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})

	if got := p.PickAnyForRealm("cn"); got != nil {
		t.Fatalf("没有 CN 号时不应返回其它域的账号，got uid=%s realm=%s", got.UID, realmOf(got))
	}
	if got := p.PickAnyForRealm("global"); got == nil || got.UID != "g1" {
		t.Fatalf("应返回 global 号，got %+v", got)
	}
}

// 禁用号不参与回退：禁用的号是「需要重新登录」的终态，拿它探测必然失败。
func TestPickAnyForRealmSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	p.Disable("cn1", "request illegal")

	got := p.PickAnyForRealm("cn")
	if got == nil || got.UID != "cn2" {
		t.Fatalf("应跳过禁用号返回 cn2，got %+v", got)
	}
}

// 冷却到期后健康口径恢复，回退不再是唯一出路（确认回退不改变原有语义）。
func TestPickAnyForRealmDoesNotAffectHealthyPath(t *testing.T) {
	p := realmPool(t)

	if got := p.PickExcludingForRealm(nil, "", "cn"); got == nil {
		t.Fatal("健康号应能被常规口径选中")
	}
	if got := p.PickAnyForRealm("cn"); got == nil {
		t.Fatal("健康号也应能被回退口径选中")
	}
}
