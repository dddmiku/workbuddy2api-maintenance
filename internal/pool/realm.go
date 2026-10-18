// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// PickAnyForRealm 按 realm 取一个**仅未禁用**的账号，忽略冷却/熔断/积分状态。
//
// 只给「读目录」这类不消耗账号额度的调用方用（模型列表探测、倍率同步）。
// 为什么需要它：目录探测此前复用 PickExcludingForRealm（健康口径），一旦该域所有
// 账号都在冷却（实测 dev 的 6 个 CN 号同时处于 hard_credit 冷却），探测直接返回
// nil → /v1/models 一个 CN 模型都不公布，客户端看起来像「模型全没了」，而实际上
// 上游目录一直是好的（同一账号手动拉 /v3/config 拿到 37 个模型）。
//
// 读目录是只读探测，不扣额度、不影响账号状态；用被冷却的账号去读是安全的，
// 也远好过对外公布空列表。realm=="" 时不做域过滤（现状语义）。
func (p *Pool) PickAnyForRealm(realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 遍历 map 取第一个可用者：读目录不关心选谁，也不要求确定性顺序。
	for _, e := range p.byUID {
		if e == nil || e.disabled || e.a == nil {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		a := e.a.Snapshot()
		return a
	}
	return nil
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}
