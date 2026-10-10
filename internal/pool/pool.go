// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
// ═══ 更新日志 ═══
// 2026-10-10：新增 SetPaidModels/paidModels：付费模型（倍率>0）的快过期优先硬过滤。
// 2026-09-19：按模型和区域保存有界探索游标，避免免费观测让其他可用账号永久饿死。
// 2026-09-18：记录后台落盘退出信号，确保 Close 返回后不会再有旧进程的后台写入。
// 2026-09-18：显式 Add 观察当前删除代次，既阻止陈旧创建意图，也允许删除后的合法重新添加。
package pool

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type Pool struct {
	mu           sync.RWMutex
	byUID        map[string]*entry
	stateFp      string
	persistBase  stateFile
	stateIntents map[string]*stateIntent
	dirty        atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// sourceGateEnabled 来源级限流闸门开关（SetSourceRateGate 注入；默认开启）。
	// 关闭后上游限流时继续换号，不按 realm 暂停选号。
	sourceGateEnabled bool
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// weightOfHook / weightOfMaxHook 仅供测试观测（DeptestOnly）：分别统计 weightOf
	// 被调次数与收到的 maxCredits 口径，验证「单次 pick 只算一次 + 全集口径」的重构
	// 契约（TestWeightOfCalledOncePerPick / TestWeightOfMaxCreditsPassedVerbatim）。
	// 生产恒 nil，零开销（nil 函数调用分支预测友好）。
	weightOfHook    func()
	weightOfMaxHook func(maxCredits int64)
	// pickSeq 选号单调序号源：仅 pick 在持 p.mu 写锁时自增并赋给 entry.usedSeq，
	// 无需 atomic。见 entry.usedSeq 注释（解决 Windows 时钟精度导致的 LRU 失效）。
	pickSeq uint64
	// costExploration only advances on ordinary selection with both free and
	// unknown candidates. Sticky requests and other models do not consume it.
	costExploration map[[32]byte]*explorationState
	// sourceRateGates 来源级限流闸门（见 sourcerate.go）：按 realm 记录「无重置时间的
	// 429」在窗口内打中的不同账号集合，达到阈值即暂停该 realm 的选号一段时间。
	// 挂在 Pool 实例上而不是包级变量：闸门是这一池账号的观测，跨 Pool 实例（含测试）
	// 共享会让互不相关的场景互相干扰。
	sourceRateGates map[string]*sourceRateGate
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh      chan struct{}
	flusherDone chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
	// slotWaitMu/slotWaitCh 在途名额释放广播：Release 真正扣减名额后关闭当前
	// 通道并置 nil，唤醒全部等待者；SlotFreed 在无通道时惰性新建。
	//
	// 用「关闭并置 nil」而不是向通道投递值：多个等待者被同一次释放唤醒后
	// 各自重跑选号，由 Acquire 的 CAS 决出谁真正拿到名额——池里因此不需要
	// 维护等待队列，也不存在唤醒丢失。惰性新建让「无等待者」时零分配：
	// Release 是最热的路径（每个请求结束都走），不能为此付出固定开销。
	slotWaitMu sync.Mutex
	slotWaitCh chan struct{}

	// paidModels 有积分倍率的模型集合（倍率 > 0 = 会扣积分，见 SetPaidModels）。
	// 付费模型上启用「快过期优先」硬过滤：积分到期即作废，付费用掉比留着强。
	// nil = 未注入（启动早期 / 测试）→ 该特性整体不生效，退化为既有行为。
	//
	// 为什么不直接复用 modelCost 实测账本：实测账本要跑过几次才有观测，新模型上线
	// 的头几个请求拿不到偏好；而倍率是上游目录里的声明，冷启动即有。两者语义也不同
	// （倍率=官方定价，实测=真实扣费，后者还受限免活动影响）。
	paidModels map[string]bool
}

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		sourceGateEnabled:  sourceGateDefault,
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetPaidModels 注入「有积分倍率（倍率 > 0）」的模型集合，供付费模型的快过期优先
// 硬过滤使用（见 paidModels 字段注释）。传空切片即关闭该特性。
//
// 模型名按裸名（不带 realm 前缀）匹配：handler 剥前缀后传给 pick 的正是裸名，
// 而上游两域同名模型的倍率可能不同（实测 cn:deepseek-v4.1-flash=x0.11、
// global:deepseek-v4.1-flash=x0.00），所以键用「realm + 裸名」，避免 CN 的付费
// 判定把 global 的免费模型也当成付费。
func (p *Pool) SetPaidModels(models map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(models) == 0 {
		p.paidModels = nil
		return
	}
	copied := make(map[string]bool, len(models))
	for k, v := range models {
		copied[k] = v
	}
	p.paidModels = copied
}

// PaidModelSummary 报告已登记的付费模型（倍率 > 0）摘要，供 /status 展示——
// 运维据此确认「付费模型优先消耗快过期积分」是否真的生效（看不到就会怀疑特性没跑）。
//
// 返回总数与排序后的键。键里的 realm 前缀**保留**（展示为 "cn:m" 而不是内部拼法）：
// 同名模型两域倍率可能不同（cn:deepseek-v4.1-flash 付费 / global 同名免费），
// 只报裸名会让运维以为 global 也被判成付费了。
func (p *Pool) PaidModelSummary() map[string]any {
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := make([]string, 0, len(p.paidModels))
	for k := range p.paidModels {
		keys = append(keys, strings.Replace(k, "\x00", ":", 1))
	}
	sort.Strings(keys)
	return map[string]any{"count": len(keys), "models": keys}
}

// paidModelKey 付费判定用的键：realm + 裸模型名。空 realm（未分池调用）时退化为
// 只看裸名——那种调用不区分域，用任一域的判定都比完全不判更接近意图。
func paidModelKey(realm, model string) string {
	if realm == "" {
		return model
	}
	return realm + "\x00" + model
}

// modelIsPaid 报告该 (realm, 模型) 是否为付费模型（有正倍率）。
// 未注入集合时恒 false——特性整体关闭，行为与引入前一致。
func (p *Pool) modelIsPaid(realm, model string) bool {
	if model == "" || p.paidModels == nil {
		return false
	}
	if p.paidModels[paidModelKey(realm, model)] {
		return true
	}
	// realm 精确键未命中时再试裸名：SetPaidModels 的调用方可能只按裸名登记。
	return p.paidModels[model]
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	limit := p.maxInFlight
	p.mu.RUnlock()
	if !ok {
		return false
	}
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			// 只有真正扣减成功才广播：幂等分支（cur<=0）没有腾出名额，
			// 唤醒等待者只会让它们白跑一轮选号。
			p.broadcastSlotFreed()
			return
		}
	}
}

// SlotFreed 返回一个在当前时刻之后「有账号释放在途名额」时被关闭的通道。
//
// 调用方语义：拿到通道后立刻重新选号；若选号仍然失败且失败原因是
// in_flight_full，则等待该通道被关闭（或 ctx / 超时先到）后重试。
//
// 为什么要先取通道再选号：若先选号再取通道，在两步之间发生的 Release 会
// 关闭旧通道、而调用方已经错过它，于是白等到超时——这正是"健康账号全忙"
// 场景下最坏情况。先取通道则保证两步之间的任何释放都会让随后的等待立即返回。
//
// 通道在无等待者时不预先分配；每次 Release 关闭并置 nil，下次调用新建。
func (p *Pool) SlotFreed() <-chan struct{} {
	p.slotWaitMu.Lock()
	defer p.slotWaitMu.Unlock()
	if p.slotWaitCh == nil {
		p.slotWaitCh = make(chan struct{})
	}
	return p.slotWaitCh
}

// broadcastSlotFreed 关闭当前等待通道唤醒全部等待者；无等待者时是空操作。
func (p *Pool) broadcastSlotFreed() {
	p.slotWaitMu.Lock()
	if p.slotWaitCh != nil {
		close(p.slotWaitCh)
		p.slotWaitCh = nil
	}
	p.slotWaitMu.Unlock()
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			change := p.stateIntentLocked(uid)
			change.deleted, change.deletedEpoch = true, 0
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	epoch, persisted, observed := p.observeAccountEpochLocked(a.UID)
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		if p.stateFp != "" && observed && (!persisted || epoch != p.persistBase.AccountEpochs[a.UID]) {
			change := p.stateIntentLocked(a.UID)
			change.created, change.deleted, change.createdEpoch = true, false, epoch
		}
		return
	}
	p.byUID[a.UID] = &entry{a: a}
	if p.stateFp != "" {
		if p.persistBase.Accounts == nil {
			p.persistBase.Accounts = make(map[string]stateAccount)
		}
		p.persistBase.Accounts[a.UID] = stateAccount{}
		change := p.stateIntentLocked(a.UID)
		change.created, change.deleted, change.createdEpoch = true, false, epoch
	}
}
