// ═══ 更新日志 ═══
// 2026-09-23：新增「来源级限流闸门」：上游 14003（too many requests，无重置时间）
// 在短时间内打中多个不同账号时，判定为出口/来源级限流，按 realm 暂停选号一段时间，
// 不再把账号一个个送去撞墙。闸门挂在 Pool 实例上，不跨实例共享。
// sourcerate.go 来源级限流闸门。
//
// ── 为什么需要它（实测依据） ──
//
// 2026-09-23 15:33–15:35 的实测：15 次 14003 落在 **12 个不同账号**上，而单账号请求
// 频率只有每分钟 2–13 次。账号级限流不会这样跨号同时命中；这是上游按来源（出口 IP /
// 客户端指纹）做的整体限流。
//
// 旧行为：每次 429 都当作账号问题 → 冷却该号 + 换下一个号重试（MaxRotate=3）。于是
// 一次客户端请求最多烧掉 3 个账号，越撞越猛，最终以
// `exceeded retry limit, last status: 429 Too Many Requests` 收场。
//
// ── 判据与出口 ──
//
// 同一 realm 在 sourceRateWindow 内累计 sourceRateThreshold 个**不同账号**命中
// 「无重置时间的 429」即触发闸门，暂停该 realm 的选号 sourceRateCooldown。
// 带重置时间的 6004 不参与：它是模型级限额，按既有模型豁免切号即可，不是来源问题。
package pool

import "time"

const (
	// sourceRateThreshold 触发闸门所需的不同账号数。
	// 取 3：单账号偶发 429 仍按原逻辑冷却换号（不误伤），三个不同号同时中招就
	// 足以说明问题不在账号上。
	sourceRateThreshold = 3
	// sourceRateWindow 统计窗口：多久之内的命中算同一波。
	sourceRateWindow = 60 * time.Second
	// sourceRateCooldown 闸门触发后暂停选号的时长。取 30s：足够让上游的频率窗口
	// 滑过去，又不至于让用户等太久；到期后放行一个请求做半开探测。
	sourceRateCooldown = 30 * time.Second
)

// sourceRateGate 单个 realm 的来源级限流闸门状态。
type sourceRateGate struct {
	// hits 窗口内命中的账号集合（用集合而非计数：同一账号反复 429 只算一次，
	// 否则一个坏号就能把闸门顶开）。
	hits map[string]time.Time
	// until 闸门解除时刻。
	until time.Time
}

// NoteSourceRateLimit 记录一次「无重置时间的 429」命中，返回闸门是否已触发。
// realm 为空时按 cn 归并（裸模型名的域）。
func (p *Pool) NoteSourceRateLimit(realm, uid string) bool {
	key := realmKeyOr(realm)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sourceRateGates == nil {
		p.sourceRateGates = map[string]*sourceRateGate{}
	}
	gate := p.sourceRateGates[key]
	if gate == nil {
		gate = &sourceRateGate{hits: map[string]time.Time{}}
		p.sourceRateGates[key] = gate
	}
	// 窗口外的旧命中先清掉，再记账。
	for account, at := range gate.hits {
		if now.Sub(at) > sourceRateWindow {
			delete(gate.hits, account)
		}
	}
	gate.hits[uid] = now
	if len(gate.hits) < sourceRateThreshold {
		return false
	}
	gate.until = now.Add(sourceRateCooldown)
	gate.hits = map[string]time.Time{}
	return true
}

// SourceRateGate 报告该 realm 当前是否处于来源级限流闸门内，以及剩余时长。
//
// 用途仅限**同一请求内的轮转决策**：闸门说明「现在再打上游也是白打」，handler 据此
// 提前收手并如实回 429，而不是把剩余账号一个个送去撞墙。它不是全局熔断——新请求
// 仍会正常选号（上游限流通常只持续很短一段时间，一刀切拒绝所有新请求反而伤可用性）。
func (p *Pool) SourceRateGate(realm string) (bool, time.Duration) {
	key := realmKeyOr(realm)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	gate := p.sourceRateGates[key]
	if gate == nil || gate.until.IsZero() {
		return false, 0
	}
	if !now.Before(gate.until) {
		// 到期：清掉闸门，放行请求做半开探测。探测若再命中会重新累计。
		gate.until = time.Time{}
		return false, 0
	}
	return true, gate.until.Sub(now)
}

// realmKeyOr 归一 realm 键：空串与未知值都归到 cn（裸模型名的默认域）。
func realmKeyOr(realm string) string {
	switch realm {
	case "global":
		return "global"
	default:
		return "cn"
	}
}
