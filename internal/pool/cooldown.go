// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// 软冷却封顶、熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
// ═══ 更新日志 ═══
// 2026-09-25：迟到的在途错误只记录活动，不覆盖禁用状态；软冷却不能解除仍有效的余额不足限制。
// 2026-09-24：首轮软冷却同样遵守封顶，账号级冷却保留独立模型限制，防止短冷却抹掉长配额。
// 2026-09-24：模型负缓存过期后保留有界退避历史，避免每次半开失败都回到六小时。
// 2026-09-20：冷却入口补记 last_err。此前只有 5xx 的 NoteError 写「最近活动」，
//
//	429 / 14017 / 余额耗尽 / 404 等冷却路径都不写，面板出现「冷却中却
//	从未活动」的自相矛盾显示（用户实测反馈）。只写时间戳，不喂 errTotal /
//	errorEMA / fails——限流与授权故障是配额信号，不是账号失败。
//
// 2026-09-18：为余额赋值及冷却/模型操作记录字段意图，多实例合并时保留其他账号与模型的独立更新。
package pool

import (
	"strings"
	"time"
)

// markCoolErrorLocked 记录一次「触发冷却的上游错误」的观测时间，供 /status 与面板的
// 「最近活动」列展示。只写 lastErr，**不**累计 errTotal / errorEMA / fails：429 限流、
// 14017 trial 未激活、6004 模型限流都是配额或授权信号，不是账号失败；喂进成功率权重会
// 让健康号因限流被持续降权，与 applyErrorPolicy「软限流不喂熔断」的既有口径冲突。
// 调用方必须已持有 p.mu。
func (p *Pool) markCoolErrorLocked(uid string, e *entry, now time.Time) {
	e.lastErr = now
	p.markStateFieldsLocked(uid, "last_err")
}

func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		p.markStateFieldsLocked(uid, "credits")
	}
}

// SetCreditsDetailed 更新账号余额总量 + 快过架子集（签到时调用，供优先消耗快过期积分）。
// expiring 会被钳到 [0, credits]：上游分桶异常时不污染权重。
func (p *Pool) SetCreditsDetailed(uid string, credits, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsExpiring = expiring
		p.markStateFieldsLocked(uid, "credits", "credits_expiring")
	}
}

// Cooldown 施加账号级冷却：只接受 CoolHard（余额耗尽到次日 04:00）。
//
// ═══ 2026-10-18：CoolSoft 账号级冷却已取消 ═══
//
// 用户要求「不要账号冷却，只要模型冷却和积分不足」。CoolSoft 的调用点已全部改走
// 模型级（CooldownSoftRateForModel / BlockModelBackoff）；这里对 CoolSoft 直接
// 忽略并只记错误观测，防止将来有人再写出账号级限流冷却。
//
// CoolHard（积分不足）保留账号级 until：它不是「冷却」，而是「什么时候再去查一次
// 积分」——上游没有限制这个账号，只是付不起付费模型；免费模型照常可用
// （见 unavailableReason 与 hardCreditBlocksModel）。
//
// 本入口不喂熔断器：熔断器只对「反复失败」（NoteError，5xx）退避。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		p.markCoolErrorLocked(uid, e, now)
		if e.disabled {
			return
		}
		if kind != CoolHard {
			// 账号级限流冷却已取消：调用方应改用模型级入口。这里不写 until，
			// 避免把整号从池里摘掉而误伤其他模型。
			return
		}
		e.until = now.Add(d)
		e.coolKind = kind
		e.reason = reason
		// 账号与模型限制正交；healthyForModel 先判账号状态，不会被模型状态绕过。
		p.markStateFieldsLocked(uid, "until", "cool_kind", "reason")
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（带解析时间）→ modelCooldowns[model].Until = resetAt（按上游实际
//     重置时间，**不再**截断到 softRateMax），ResetAt 记录上游原始墙钟（台账 ResetAt）。
//     不写 until（全账号级冷却不受模型级限流污染），切模型即可用（模型豁免）。
// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（带解析时间）→ modelCooldowns[model].Until = resetAt（按上游实际
//     重置时间，**不再**截断到 softRateMax），ResetAt 记录上游原始墙钟（台账 ResetAt）。
//     不写 until（全账号级冷却不受模型级限流污染），切模型即可用（模型豁免）。
//   - resetAt 零值（无时间文案）→ 同样记在该模型上做有界退避（base 起按该模型的命中
//     次数翻倍、封顶 softRateMax）；已在模型冷却中时不推进。
//   - model 为空 → 只记错误观测，不施加任何冷却（账号级限流冷却已于 2026-10-18 取消）。
//
// 与旧实现的差异：有上游重置时间时绝对不做指数堆加（旧实现 softStreak 计数含
// 死逻辑）；无重置时间时，「冷却中兜底探测再 429」不再翻倍——这正是用户
// 「全池被推到 2h 封顶」的元凶。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		p.markCoolErrorLocked(uid, e, now)
		if e.disabled || model == "" || (resetAt.IsZero() && e.coolKind == CoolHard && now.Before(e.until)) {
			return
		}
		if !resetAt.IsZero() {
			// 有上游重置时间：冷却截止 = min(resetAt, now+softRateMax)，不做指数放大。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:   p.cappedSoftUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason:  reason,
			}
			e.trimModelCooldowns()
			p.markStateModelLocked(uid, model)
		} else {
			// 无解析时间（文案里没给重置墙钟）：同样记在**模型**上——账号级冷却已
			// 取消（2026-10-18），限流一律是模型维度。退避按该模型的命中次数翻倍、
			// 封顶 softRateMax，且**已在模型冷却中**时不推进（兜底探测不得把冷却
			// 越堆越厚——旧实现每次都翻倍，把全池推到封顶）。
			if model != "" {
				cur, tracked := e.modelCooldowns[model]
				if !tracked || cur.Until.IsZero() || !now.Before(cur.Until) {
					hits := cur.Hits + 1
					d := p.softDurationLocked(base, hits)
					if e.modelCooldowns == nil {
						e.modelCooldowns = map[string]modelCooldown{}
					}
					e.modelCooldowns[model] = modelCooldown{
						Until:  now.Add(d),
						Hits:   hits,
						Reason: reason,
					}
					e.trimModelCooldowns()
					p.markStateModelLocked(uid, model)
				}
			}
		}
		p.dirty.Store(true)
	}
}

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口（handler.applyErrorPolicy
// 调用）。复用 modelCooldowns 机制（不新建平行状态）：写 modelCooldowns[model]，Until 为指数退避
// TTL，选号侧 healthyForModel 自动对该账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端无此模型、
// 重试无意义，只能换模型/换账号」。退避按 reference model_blocks.py 口径（吸收语义、不抄实现）：
//   - 首次命中 TTL = modelBlockBaseTTL（6h）；
//   - 半开到期后允许放行重试；再次命中 Hits++ → TTL = base × 2^min(hits-1, modelBlockShift)；
//   - 封顶 modelBlockMaxTTL（24h，最多一天再试一次）；
//   - 该模型请求成功即由 BlockModelClear 清除（注意：6004 的 NoteSuccess 不清 modelCooldowns，
//     故清理必须由 handler 在成功且 model 命中 11102 条目时显式调用，见 BlockModelClear）。
//
// resetAt 无需传（11102 无重置文案），ResetAt 保持零值，与 6004 台账（rateLimitedModelsLocked）
// 共用 Until 判定——11102 条目会以 11102 reason 出现在 /status 台账，运维可见。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	p.markCoolErrorLocked(uid, e, now)
	if e.disabled {
		return
	}
	e.pruneExpiredModelCooldowns(now)
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	e.trimModelCooldowns()
	p.markStateModelLocked(uid, model)
}

// ClearModelLimit 清除 (账号, 模型) 的**限流**记录（该模型实测又通了）。
//
// 2026-10-18：限流成为唯一的账号侧限流维度后，「成功即证明恢复」必须能清掉它。
// 此前 NoteSuccess 有意不碰 modelCooldowns——那时 6004 有上游权威重置墙钟，成功一次
// 不该抹掉「上游说要用到明天」的事实。但现在的模型级台账同时承载 6004 与无时间的
// 退避（后者本来就是「探测式」的猜测）；对退避类条目，成功就是最直接的证伪。
//
// 因此本函数清掉该模型的**限流类**条目（reason 不是 11102 负缓存；负缓存由
// BlockModelClear 单独负责，语义是「官方说没这个模型」，成功后也要清）。
// 带上游重置墙钟的 6004 条目**不清**——上游明确说该模型要到某时刻才恢复，本地成功
// 一次不足以推翻（可能是别的副本/别的通道）。
func (p *Pool) ClearModelLimit(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !mc.ResetAt.IsZero() {
		return // 有上游权威墙钟的条目不因一次成功而清除
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.markStateModelLocked(uid, model)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测或正常
// 请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立冷却表——
// 6004 有自身上游重置墙钟语义，成功不该抹掉（见 NoteSuccess 注释）。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见上游 BlockModelReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.markStateModelLocked(uid, model)
}

// CooldownSoftRateForModel 把一次限流记录在**具体模型**上（唯一的限流维度）。
//
// ═══ 2026-10-18：账号级限流冷却已删除 ═══
//
// 用户要求「不要账号冷却，只要模型冷却和积分不足」。此前存在一个不带模型的
// `CooldownSoftRate(uid, ...)`，它把 429 写进账号级 until，等于把这个号对**所有**
// 模型一起摘掉；prod 实测 30 个号里没有一个真正的账号级 429 冷却，全是积分不足，
// 说明这层状态从未被正确使用，却在命中时误伤其他模型。该函数已删除——没有模型名
// 就无法把一次限流归到具体模型上，而「归不到模型」不等于「整个账号不能用」。
//
// model 为空时本函数只记录错误观测，不施加任何冷却。
//
// 语义：
//   - resetAt 非零（上游给了权威重置墙钟）→ 该模型冷却到该时刻（按实际时间，不截断，
//     绝不指数堆加）；
//   - resetAt 为零（文案没给时间）→ 按**该模型**已有命中次数做有界退避
//     （base 起翻倍、封顶 softRateMax）。
func (p *Pool) CooldownSoftRateForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	p.markCoolErrorLocked(uid, e, now)
	if e.disabled || model == "" {
		return
	}
	// 余额不足（CoolHard）在有效期内时，不在同一账号上再叠模型冷却：积分耗尽的
	// 判定优先级更高，而且 04:00 签到后积分恢复、模型限流未必解除——两者独立成立，
	// 互不覆盖（与 CooldownSoftForModel 的既有守卫一致）。
	if !resetAt.IsZero() {
		if e.modelCooldowns == nil {
			e.modelCooldowns = map[string]modelCooldown{}
		}
		e.modelCooldowns[model] = modelCooldown{
			Until:   p.cappedSoftUntilLocked(now, resetAt),
			ResetAt: resetAt,
			Reason:  reason,
		}
		e.trimModelCooldowns()
		p.markStateModelLocked(uid, model)
	} else {
		// 无重置墙钟：按该模型已有的命中次数做有界退避（base 起、翻倍、封顶
		// softRateMax）。注意这里计的是**该模型**的次数，不是账号的 softStreak——
		// 账号级 softStreak 已随账号冷却一起停用。
		//
		// 已在模型冷却中时不推进：兜底探测/用户重试不得把冷却越堆越厚。
		cur, tracked := e.modelCooldowns[model]
		if !tracked || cur.Until.IsZero() || !now.Before(cur.Until) {
			hits := cur.Hits + 1
			d := p.softDurationLocked(base, hits)
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until:  now.Add(d),
				Hits:   hits,
				Reason: reason,
			}
			e.trimModelCooldowns()
			p.markStateModelLocked(uid, model)
		}
	}
	p.dirty.Store(true)
}

// cappedSoftUntilLocked 把上游重置墙钟换算成本地冷却截止。
//
// 2026-10-18 改口径：**按上游实际重置时间冷却，不再截断到 softRateMax**。
//
// 旧实现取 min(resetAt, now+softRateMax)，把上游「明天 02:30 重置」砍成 2 小时。
// 实测后果（prod，global:deepseek-v4.1-flash）：2 小时一到号被放回池子，立刻再撞
// 6004，再冷却 2 小时，如此往复——用户看到的是「号池里大部分号看着健康、但整个
// 模型用不了」，而真实的限流窗口是 18 小时。冷却时长必须服从上游权威墙钟，
// 否则就是自己制造「假恢复」。
//
// 保留的两条边界：
//   - resetAt 已过期（时钟偏移 / 文案陈旧）→ 立即恢复（now+1ms），不把号钉死。
//   - resetAt 离谱地远（> maxSoftResetWindow，只可能来自错误解析或严重时钟漂移）
//     → 钳到该上限，避免一个坏字符串把账号永久锁死。
//
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	if resetAt.After(now) {
		if far := now.Add(maxSoftResetWindow); resetAt.After(far) {
			return far
		}
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供**无上游重置时间**的
// 指数退避封顶使用。有上游重置时间的冷却不走这里（见 cappedSoftUntilLocked）。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算，首轮也必须遵守封顶。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	max := p.softRateMaxOr()
	if d >= max {
		return max
	}
	if streak <= 1 {
		return d
	}
	if d <= 0 {
		return max
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	if d > max>>shift {
		return max
	}
	return d << shift
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// QuarantineChannelRejected 曾实现「渠道拒绝 = 账号被标记 → 隔离 24h」（2026-10-17
// dd8dae0）。该结论已被真实客户端抓包推翻——同一批账号在官方客户端正常使用，裸探测的
// 11128 是缺官方头部与 system 首条所致（真实 msg 为 "first message is not system prompt"，
// 兜底文案 "request illegal" 掩盖了它）。函数已随错误结论一并删除，历史见 backup 分支
// backup/wrong-diagnosis-20261017。

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到后解冻：余额恢复只清余额不足冷却，保留限流和模型限制。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。

// ModelLimitedUntil 报告账号对 model 当前是否处于模型级限流，返回截止时刻与原因。
// 供面板「检测模型冷却」展示与探测判定使用；不存在或已过期返回零值。
func (p *Pool) ModelLimitedUntil(uid, model string) (time.Time, string, bool) {
	if uid == "" || model == "" {
		return time.Time{}, "", false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, "", false
	}
	mc, ok := e.modelCooldowns[model]
	if !ok || mc.Until.IsZero() || !time.Now().Before(mc.Until) {
		return time.Time{}, "", false
	}
	return mc.Until, mc.Reason, true
}
