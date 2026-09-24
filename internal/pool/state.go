// 账号状态演进与查询：禁用/12153 连续计数判定、成功与错误入账、复活解冻，
// 以及状态查询（Status/AvailableUIDs/PickByUIDForModel/CountsDetailed/ServableNow/List）。
// ═══ 更新日志 ═══
// 2026-09-24：余额恢复仅解除余额冷却，探活始终服从账号级冷却与熔断。
// 2026-09-18：跨实例落盘保留显式复活/清零意图，并以实际扣费增量合并余额，避免旧快照回滚状态。
// 2026-09-18：持久化扣费保留实际消费量，只有本地余额展示钳零，避免旧余额少记后来可见的消费。
package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

// Disable 永久禁用（session 死亡 / 账号级授权封禁），需人工重登后手工恢复或文件替换。
// 经 disableLocked：置 disabled 并清冷却域（until/coolKind/softStreak/modelCooldowns），
// 熔断器保留（熔断是连续 5xx 信号，与授权/session 正交，见 transition.go）。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.disableLocked(e, reason)
	}
}

// NoteSessionDead 记录一次 ErrSessionDead（12153）——**不立即禁用**。
// 旧行为一次 12153 即 Disable，但 12153 会被临时性触发（网络抖动/上游闪断/refresh
// 竞态），一次失败就永久杀号会误杀健康账号（P0-1 侦察：13 个 disabled 号全部 refresh
// 成功，是历史误判的受害者）。改为连续 sessionDeadThreshold 次才禁用：
// 计数 +1，达到阈值 → Disable（reason=12153 session dead）并清计数；
// refresh 成功 / 任意成功 / 手工复活 → ClearSessionDead 清计数。
// 返回 true 表示本次已达阈值并完成禁用。
// 即使账号已 disabled，计数仍累计并返回 false 前 N-1 次——但 keepalive 会跳过
// disabled 号，实际只有「已 disabled 后复活且计数未清」这类场景才会走到这里。
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		p.dirty.Store(true)
		return false
	}
	e.sessionDeadFails = 0
	p.disableLocked(e, sessionDeadReason)
	return true
}

// ClearSessionDead 清连续 12153 计数——账号被证明未死的任何时刻调用：
// refresh 成功（RunKeepaliveNow）、chat 成功（NoteSuccess）、手工复活（ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.sessionDeadFails != 0 {
		e.sessionDeadFails = 0
		p.markStateFieldsLocked(uid, "session_dead_fails")
	}
}

// ReviveDisabled 人工/端点复活入口：清除 disabled + reason + 连续 12153 计数，
// 账号回到池子（若无其他冷却/熔断则立即可选，健康检查自然接管）。
// **不改** Disabled 在选号/状态端点的既有语义：disabled 号依然不参与选号，
// 直到被本方法复活。不存在的 uid 为空操作。
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.markStateFieldsLocked(uid, "disabled", "reason", "session_dead_fails")
	}
}

// ReenableIfCredits 在余额恢复后解除余额不足冷却。余额不证明请求频率或模型配额
// 已恢复，因此软冷却、模型限制和熔断继续按各自截止生效。禁用账号只更新余额。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain)
		} else {
			e.credits = remain
			p.markStateFieldsLocked(uid, "credits")
		}
		p.dirty.Store(true)
	}
}

// NoteError 记录一次错误：喂入唯一的连续失败计数器 fails + 累计错误 errTotal，
// 并拉高 errorEMA（成功率权重的衰减口径）。达到 breakerThreshold 触发熔断（指数
// 退避），连续失败语义整体并入熔断器（不再有独立的 err 冷却）。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.errorEMA += (1 - e.errorEMA) * successAlpha
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteTransientError 记录一次「已被网关自愈、不构成账号问题」的上游错误观测时间。
// 只写 last_err（面板「最近活动」据此显示），**不**累计 errTotal / errorEMA / fails：
// 典型场景是 global 14017（缺注册地）——网关补交注册地后同号立刻恢复，账号本身没坏，
// 喂进成功率权重只会无故降权。调用方必须已确认该错误无需按失败记账。
func (p *Pool) NoteTransientError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.markCoolErrorLocked(uid, e, time.Now())
	}
}

// ModelCost 读取账号在某模型上的实测扣费观测（CostPer1k 与是否存在有效观测）。
// DeptestOnly: 生产只写不读（NoteModelCost 有调用），读取侧仅
// handler_cost_test / global_e2e_test 断言账本内容。跨包（internal/server）
// 测试引用，迁 export_test.go 不可行（对包外不可见）。
// 无观测或观测过期（modelCostTTL）时 ok=false。
func (p *Pool) ModelCost(uid, model string) (per1k float64, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, false
	}
	mc, ok := e.modelCostOf(model, time.Now())
	if !ok {
		return 0, false
	}
	return mc.CostPer1k, true
}

// NoteModelCost 记录一次实测扣费观测，更新该 (账号, 模型) 的成本账本，并顺带
// 扣减账号余额（credits/creditsExpiring，见下方「P1-A」段）。credit 为上游
// usage.credit（本次真实扣费=消耗量），tokens 为本次请求的 token 总数
// （prompt+completion，用于折算单位成本）。tokens<=0 时不记录：无法折算单价，
// 记进去会污染账本。
//
// 用 EMA 平滑（alpha=0.3，约 5 次观测收敛）：单次异常值不主导选号决策。
// 账本仅内存态——成本随上游活动（限免期/夜间免费/折扣）变化，持久化旧值
// 反而是脏数据；重启后重新学习，代价只是前几次请求无偏好。
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 {
		return
	}
	// 单价按每千 token 归一，消除请求长度差异。
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	// P1-A credits 签到外回写：credit 是本次请求的**消耗量**（上游 usage.credit，
	// handler 侧 stats.Credit()/usageCreditTotal），不是剩余余额。顺手扣减 credits
	// 与 creditsExpiring，让四因子里的两个余额因子随消耗实时收敛——旧口径只在
	// 签到（每天 09:00/21:00 两次）刷新，两次签到之间（最长 12h）高消耗号持续
	// 高权重直到打空撞 402；global 账号不签到，credits 曾是终身冻结。
	// 签到仍定期覆盖（ReenableIfCredits/SetCreditsDetailed 以 authoritative 余额
	// 重置），扣减只是两次签到之间的内插估计；credit=0（免费请求）不动余额。
	if credit > 0 {
		consumed := int64(credit + 0.5) // 四舍五入，与测试口径一致（2.5 → 3）
		d := consumed
		if d > e.credits {
			d = e.credits // 钳 0：扣穿（对账延迟/消费早于记账）不产生负余额
		}
		e.credits -= d
		p.addStateDeltaLocked(uid, "credits", -consumed)
		p.addStateDeltaLocked(uid, "credits_expiring", -consumed)
		if e.creditsExpiring > 0 {
			if d > e.creditsExpiring {
				d = e.creditsExpiring
			}
			e.creditsExpiring -= d
		}
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
}

// NoteSuccess 成功请求累加成功计数、刷新 lastSuccess，并清空连续失败与熔断运行态。
// 二进制模型：清 fails + retryCount + breakerUntil；不碰 until/coolKind（那些是即时冷却，各自到期）。
// 额外清 softStreak：成功是账号已恢复的最强证据，连续软限流计数就此归零、退避回到基数。
// 同样清 sessionDeadFails：成功证明 session 未死（与 ClearSessionDead 语义一致）。
// **不碰 modelCooldowns**：6004 模型级 limit 每模型独立计时，其他模型成功不得抹掉
// 本模型的冷却截止（这正是"每模型独立"的语义）。模型级冷却由到期/禁用清理；
// 11102 负缓存另由对应模型请求成功清除，余额恢复不能清除。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.successEMA += (1 - e.successEMA) * successAlpha
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		p.markStateFieldsLocked(uid, "breaker_until", "retry_count", "soft_streak", "session_dead_fails")
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs 返回当前 healthy 且未占满在途名额的账号 UID 列表（按 UID 排序，稳定输出）。
// 供会话粘性路由（internal/session）做快路径命中校验 + 双段分配；无可用返回空切片。
func (p *Pool) AvailableUIDs() []string {
	return p.availableUIDsLocked("", func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModel 同 AvailableUIDs，但把健康口径换成 healthyForModel：
// 在该模型上被 6004 限流的账号不列入，而在**其他模型**被限流的账号照常列入
// （issue #31 模型豁免）。
// DeptestOnly: 仅 cost_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm（带 realm 维度）。保留作 ForModelRealm 的
// realm=="" 退化语义锚点测试。
// 供会话粘性按模型分配与命中校验；model 为空时等价于 AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	return p.availableUIDsLocked("",
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// availableUIDsLocked 是 AvailableUIDs 四变体（AvailableUIDs/ForModel/ForRealm/
// ForModelRealm）共用的遍历实现：realm 过滤（""=全池）+ 可替换健康口径（healthy /
// healthyForModel）+ 在途占满过滤，输出按 UID 排序（稳定）。调用方必须不持锁。
func (p *Pool) availableUIDsLocked(realm string, health func(e *entry, now time.Time) bool) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !health(e, now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel 若 uid 当前 healthy（含模型级 6004 豁免口径）且未占满在途名额，
// 返回其凭证（记录 lastUsed 防撞号）；否则返回 nil。供会话粘性路由命中校验与直取使用。
// 绑定号在当前模型被 6004 限流时返回 nil，让调用方（handler）解绑并回落普通轮换——
// 这是粘性能"换得动"的关键：绑定只记 uid，若只按账号级 healthy 校验，
// 被模型级限额的号（账号整体仍健康）会被持续选中直到轮换次数耗尽。
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	// 粘性路径同样推进 usedSeq/pickSeq：粘性重度使用的账号在 LRU 兜底
	// （pick 按 usedSeq 选最旧）眼中不再是"最旧"，与 pick 的严格全序语义对齐
	// （entry.usedSeq 注释声明「每次被选中时取 pickSeq 自增值」，粘性命中也是选中）。
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// CountsDetailed 返回 total/healthy/cooling/disabled/inFlightFull 五类计数。
// cooling 含常规冷却（until）与熔断期（breakerUntil）。
// 注意：healthy 口径不含 inFlight 维度（是状态机权威判定，只看 disabled/until/breakerUntil）；
// inFlightFull 是 healthy 的子集——healthy 里已达在途上限的账号数，供 /status 透出满载度。
// 与 ServableNow 的区别见该函数注释。
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm 同 CountsDetailed，但仅统计 Realm()==realm 的账号；
// realm=="" 退化为全池（现状语义，走同一遍历 helper 避免重复代码）。
// 双 realm 共存时供 /status 按域分组暴露 CN/global 各自可用性。
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm 是两函数共用的遍历实现；realm=="" 不加谓词。
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow 报告池当前是否可服务：存在至少一个（对任意模型）healthy 且未占满在途名额的账号。
// 与 CountsDetailed 的 healthy 口径不同：healthy 只看 disabled/until/breakerUntil（状态机权威判定），
// 不看 inFlight；ServableNow 额外叠加在途维度，与 chat 的真实可达性（Pick 会跳过 inFlightFull 账号）对齐。
// 专供 /healthz 用，避免"全账号 healthy 但都占满"时探活误报 200 而 chat 返回 503 的口径裂缝。
//
// 单模型限制不改变 healthy，因此仍可服务其他模型；并发错误产生账号冷却与模型
// 限制并存时，必须服从账号级冷却，不能让模型记录反过来制造可用性。
func (p *Pool) ServableNow() bool {
	return p.servableLocked("")
}

// ServableForRealm 报告某 realm 是否可服务：存在至少一个该 realm 的 healthy 且未占满在途名额的账号。
// 与 ServableNow 同口径（healthy 且未占满在途），仅叠加 Realm()==realm 谓词。
// realm=="" 退化为 ServableNow（现状语义）。供 /healthz 按 realm 暴露 CN/global 各自可达性。
func (p *Pool) ServableForRealm(realm string) bool {
	return p.servableLocked(realm)
}

// RealmRateState 描述某 realm 在指定模型上的可达性与限流占用，供 global→CN 同名模型回落判定。
//
// 语义分工：
//   - Accounts：该 realm 的账号总数（判定「这个域到底有没有号」）。
//   - Available：真正可服务（healthyForModel 且未占满在途名额）的账号数。
//   - RateLimited：因**限流**不可用的账号数——账号级软冷却（429），或该模型的
//     6004 模型级冷却。
//   - Disabled：已禁用（永远不会服务）的账号数。
//
// 回落的充分条件（见 handler）：Accounts>0、Available==0、
// 且 RateLimited+Disabled==Accounts —— 即「这个域一个能用的号都没有，且所有不能用的
// 号要么被限流、要么已禁用」。只要存在因熔断/在途占满而不可用的号，就不满足该等式，
// 回落不会发生：那些是暂时或与限流无关的状态，不该被静默改道到另一个域。
type RealmRateState struct {
	Accounts    int
	Available   int
	RateLimited int
	Disabled    int
}

// AllRateLimited 报告是否满足「该域无可服务号、且不可用的号全部因限流或禁用」。
func (s RealmRateState) AllRateLimited() bool {
	if s.Accounts == 0 || s.Available != 0 || s.RateLimited == 0 {
		return false
	}
	return s.RateLimited+s.Disabled == s.Accounts
}

// RealmRateStateForModel 统计某 realm 在指定模型上的可达性与限流占用。
// realm=="" 统计全池；model=="" 时模型级冷却不参与判定（退化为账号级口径）。
func (p *Pool) RealmRateStateForModel(realm, model string) RealmRateState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var state RealmRateState
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		state.Accounts++
		if e.disabled {
			state.Disabled++
			continue
		}
		if e.healthyForModel(now, model) {
			if !p.inFlightFull(e) {
				state.Available++
			}
			continue
		}
		// 不可用：区分「限流」与「熔断」等其它原因——只有限流计入 RateLimited。
		if e.modelCooled(now, model) {
			state.RateLimited++
			continue
		}
		if e.coolKind == CoolSoft && !e.until.IsZero() && now.Before(e.until) {
			state.RateLimited++
		}
	}
	return state
}

// servableLocked 是 ServableNow / ServableForRealm 共用的遍历实现：
// 存在至少一个（realm 匹配、未占满在途名额、healthy）的账号即 true。
// realm=="" 不加 realm 谓词（全池）。调用方必须不持锁。
func (p *Pool) servableLocked(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if e.healthy(now) {
			return true
		}
	}
	return false
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	// reason 过期清理：非 disabled 账号若 until 已过期/零值，reason 清空（与落盘
	// 清理 cooledReasonLocked 同口径）。disabled 账号的 reason 是禁用原因，保留。
	_, reason := cooledReasonLocked(e, now)
	st := Status{
		UID: uid,
		// 限额台账（issue #36）：仅「带解析时间 6004 的模型级软冷却」仍在生效时非空，
		// 每模型一行（modelCooldowns 内未到期的条目），多模型同时限流全部展示。
		// 到期判据 = 该模型的独立冷却 until 未过；条件满足才输出，随到期自然消失，
		// 普通软冷却（无模型级表）/硬冷却不产生台账（零回归）。
		RateLimitedModels: p.rateLimitedModelsLocked(e, now),
		Realm:             e.a.Realm(),
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		Cooling:           now.Before(e.until) || now.Before(e.breakerUntil),
		Reason:            reason,
		Disabled:          e.disabled,
		SuccessCount:      e.successCount,
		ErrTotal:          e.errTotal,
		LastSuccessTime:   e.lastSuccess,
		LastErrTime:       e.lastErr,
		Until:             e.until,
		SoftStreak:        e.softStreak,
		InFlight:          int(e.inFlight.Load()),
		BreakerFails:      e.fails,
		BreakerUntil:      e.breakerUntil,
	}
	if st.Disabled {
		// 禁用账号透出禁用原因（运维看不到为什么死）。
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// 冷却剩余秒数（向上取整，避免 0 显示为已到期）。口径与 Cooling 判定一致：
		// 取 until 与 breakerUntil 中更远的截止（发现 5——熔断冷却的号原实现只算
		// until，显示"冷却中却 0 秒恢复"；BreakerUntil 虽单独透出，两口径不一致
		// 误导排查）。两者都过期不会进入本分支（Cooling=false）。
		remain := time.Until(e.until)
		if b := time.Until(e.breakerUntil); b > remain {
			remain = b
		}
		st.CoolRemaining = int64(remain.Seconds() + 0.999)
		if st.CoolRemaining < 0 {
			st.CoolRemaining = 0
		}
		st.CoolKind = e.coolKind.String()
	}
	return st
}

// rateLimitedModelsLocked 构建单账号的限额台账行，从 modelCooldowns 遍历输出——
// 每模型一行（含该模型的独立 until + 上游原始 resetAt），多模型同时 6004 全部展示。
// 有效期判据 = 该模型的独立冷却 until 未过；随到期自然消失（与 /status 观感一致）。
// 无模型级冷却（普通软冷却/硬冷却）→ nil（零回归）。调用方必须已持有 p.mu。
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// 先排序模型名，保证 /status 输出稳定（map 遍历无序）。
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			row := RateLimitedModel{
				Model:  m,
				Until:  mc.Until,
				Reason: mc.Reason,
			}
			// 上游「将在 … 重置」的原始墙钟：无论是否被 soft_rate_max 截断都透出——
			// 未截断时 Until==ResetAt（两者同值），截断时 ResetAt 是真实恢复时刻，
			// 台账据此始终可见上游权威时点（omitempty 仅在无 ResetAt 的旧数据上省略）。
			if !mc.ResetAt.IsZero() {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------
