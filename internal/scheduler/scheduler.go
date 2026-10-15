// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 /
// 连登兑换 / 成长抽奖 / 补签 —— 多类独立排程，各自独立开关与独立时点。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// ═══ 更新日志 ═══
// 2026-10-10：签到跳过 global 号时仍查余额，使 credits_expiring 对全部账号可用。
// 2026-09-28：保活对停用号探活，refresh 成功即自动复活（避免误停用后永远靠人工）。
// 2026-09-24：签到预刷新成功清除旧会话失效计数，避免间断 12153 累积成永久禁用。
// 2026-09-17：保留较新调度上下文及奖励幂等，统一凭据快照读取。
// 2026-09-16：定时任务的凭据存在性判断改读快照，避免与聊天触发的刷新并发竞争。
// ═══ 更新日志 ═══
// 2026-09-18：排队任务在获取执行锁后再次检查取消，脚本类任务传递排程生命周期上下文。
// 2026-09-18：签到和刷新在取消后停止后续请求，已完成的凭据刷新仍先落盘再退出。
// 2026-09-18：统一手动与定时任务生命周期，Run 结束前取消并等待所有已接受的后台任务。
// 2026-09-18：Run 在任务锁内补齐零值生命周期和任务映射，避免空取消回调崩溃且不替换已有任务上下文。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即各类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHours []int // 默认 [9, 21]
	// TraeCheckHours 是 Trae 域签到的独立排程。Trae 与 WorkBuddy 是**两个不同的
	// 站点**（不同端点、不同风控），签到必须分开：混在一个任务里无法单独开关，
	// 面板也会把 trae 归到 WorkBuddy 的「签到」下。
	TraeCheckHours []int // 默认 [10]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	SchoolHours    []int // 默认 [12]：开学季任务（迁移自系统 crontab）
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	// GrowthHours 成长中心流水线排程（原 redeem/lottery/makeup 三条合并为一条）。
	// 默认 [9,21]：早晚各跑一轮完整七段（旅行/任务奖/补登/兑换/抽奖/Buddy盲盒/展示）。
	// 合并的理由见 internal/config/schedule.go 的 GrowthHours 注释。
	GrowthHours []int
	// BalanceHours 余额刷新的独立排程。默认与签到同小时，但**独立开关**：
	// 多客户端共用账号的用户要关签到，却仍需要余额数据来驱动「快过期优先消耗」。
	// 详见 internal/config/schedule.go 的 BalanceHours 注释。
	BalanceHours []int // 默认 [9,21]
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TraeCheckDisabled 显式关闭 Trae 签到排程（schedule.trae_check_enabled=false）。
	TraeCheckDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// SchoolDisabled 显式关闭开学季任务排程（schedule.school_enabled=false）。
	SchoolDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
	// GrowthDisabled 显式关闭成长中心流水线（schedule.growth_enabled=false）。
	GrowthDisabled bool
	// BalanceDisabled 显式关闭余额刷新排程（schedule.balance_enabled=false）。
	// 关掉后池里的 credits / credits_expiring 不再更新，「快过期优先消耗」随之失效。
	BalanceDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// traeDeviceGen Trae 签到的设备号代数：uid → gen。9074 是设备维度限流，
	// 换新派生的设备号即可绕过（实测第 0 代 9074、第 1 代立刻成功）。命中后
	// 代数 +1 并重试一次，成功后记住新代数。进程重启即清零，不持久化。
	traeDeviceGen map[string]int
	// traeBackoff Trae 签到的 9074 有界退避：uid → {下次可试时刻, 连续次数}。
	//
	// 轮换设备号能绕过大多数 9074，但**绕不过上游的高峰期**：实测 2026-10-12
	// 00:00（新的一天、跨天边界）两个号连 gen0→gen1 都吃 9074。参考实现
	// （autumnsentiment/Trae2api-cn）同样保留一层有界退避兜住这种情况。
	// 基数 60s、每次翻倍、指数封顶 3（最多 8 分钟），而不是线性重试——上游的
	// 限流是「人多」这种容量信号，短时间猛重试只会让窗口更长。
	traeBackoff map[string]traeBackoffEntry
	// traeLastClaim 上一次发出 claim 的时刻（全局，不分账号）。两次之间至少隔
	// traeClaimGap，让各账号的尝试分散到不同的上游窗口里。
	traeLastClaim time.Time

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex

	// taskMu/taskLast/taskBusy 任务自省状态：供 /tasks 与账户管理面板读取"上次完成时刻"
	// 与"是否正在跑"，并由 beginTask 统一做定时入口与手动触发的互斥。
	// 只存内存、重启即清零（与 adoptTried 同口径：这些是观测值，不是要持久化的业务状态）。
	taskMu    sync.Mutex
	taskLast  map[TaskKey]time.Time
	taskBusy  map[TaskKey]bool
	taskWG    sync.WaitGroup
	stopping  bool
	lifecycle context.Context
	cancel    context.CancelFunc

	// disabledOverride 是**运行期**的任务开关覆盖：TaskKey → 是否停用。
	//
	// 为什么需要它（2026-10-15 用户反馈「任务启停都要重启网关才行，这合理吗」）：
	// 面板改 config.json 只写了文件，而排程器在启动时把 schedule.*_enabled 抄进了
	// Config 的 CheckinDisabled 等字段——改完不重启，运行中的排程器仍按旧值派发。
	// 用户点「停用」看到「已保存（重启后生效）」，实际要手动重启才生效。
	//
	// 有了覆盖表，SetTaskEnabled 之后网关可以**原地**改这一个键，下一次 nextWake
	// 就按新值算时点（Run 主循环每轮重新调用 nextWake），无需重启进程。
	// 只存内存：它是「当前生效值」，真值仍是 config.json，重启后从配置重建。
	disabledOverride map[TaskKey]bool

	// wake 是唤醒主循环的信号通道（容量 1，非阻塞投递）。
	//
	// 改了开关之后必须把正在 sleep 的 timer 叫醒：否则「启用」一个任务后，主循环
	// 还在按旧时点（可能几小时后）睡着，用户看到「已启用」却半天不跑。
	wake chan struct{}

	// runMu 全局任务互斥：同一时刻只允许一类任务在跑。这些任务打的是同一批上游
	// 账号，并发只会让风控更容易命中；顺带让任务日志有唯一归属者，
	// taskLogSink.cur 因此不必处理多重归属。
	runMu sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TraeCheckHours) == 0 {
		cfg.TraeCheckHours = []int{10}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.SchoolHours) == 0 {
		cfg.SchoolHours = []int{12}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	if len(cfg.GrowthHours) == 0 {
		cfg.GrowthHours = []int{9, 21}
	}
	// 与 config.Normalize 保持同一份默认（两处都要有：前者给 config.json 路径，
	// 这里给直接构造 Config 的调用方与测试——漏了会让 nextFire 拿到空切片、
	// 返回零时点，任务在面板上「启用但没有下次运行时间」）。
	if len(cfg.BalanceHours) == 0 {
		cfg.BalanceHours = []int{9, 21}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	// #nosec G118 -- cancel 存进 Scheduler.cancel，由 Run 在退出时调用（见同文件 defer cancel()），不是漏调用
	lifecycle, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		cfg:              cfg,
		lifecycle:        lifecycle,
		cancel:           cancel,
		adoptTried:       make(map[string]string),
		taskLast:         make(map[TaskKey]time.Time),
		taskBusy:         make(map[TaskKey]bool),
		disabledOverride: make(map[TaskKey]bool),
		wake:             make(chan struct{}, 1),
	}
}

// taskDisabled 报告某类任务当前是否停用：运行期覆盖优先，其次看启动时的配置。
//
// 调用方必须已持有 taskMu（覆盖表由它保护）。
func (s *Scheduler) taskDisabled(k taskKind) bool {
	if disabled, ok := s.disabledOverride[k.key()]; ok {
		return disabled
	}
	switch k {
	case taskCheckin:
		return s.cfg.CheckinDisabled
	case taskTraeCheck:
		return s.cfg.TraeCheckDisabled
	case taskTravel:
		return s.cfg.TravelDisabled
	case taskActivity:
		return s.cfg.ActivityDisabled
	case taskKeepalive:
		return s.cfg.KeepaliveDisabled
	case taskSchool:
		return s.cfg.SchoolDisabled
	case taskCat:
		return s.cfg.CatDisabled
	case taskGrowth:
		return s.cfg.GrowthDisabled
	case taskBalance:
		return s.cfg.BalanceDisabled
	}
	return false
}

// SetTaskEnabled 原地改一个任务的启停，并唤醒主循环重算下次运行时点。
//
// 返回 false 表示任务标识未知（调用方据此报错，不写任何状态）。
// 这是「面板改开关不用重启网关」的落点：真值仍在 config.json（面板负责写文件），
// 这里只把**当前生效值**跟上，让排程立即按新值走。
func (s *Scheduler) SetTaskEnabled(key string, enabled bool) bool {
	kind, ok := kindOf(key)
	if !ok {
		return false
	}
	s.taskMu.Lock()
	s.disabledOverride[kind.key()] = !enabled
	s.taskMu.Unlock()
	// 叫醒主循环：它此刻可能正按旧时点睡着（可能是几小时后），不叫醒就要等到那时
	// 才按新开关重算——用户会看到「已启用」却迟迟不跑。
	select {
	case s.wake <- struct{}{}:
	default: // 已有待处理信号，无需重复投递
	}
	return true
}

// TaskEnabled 报告某任务当前是否启用（供面板回显与 /tasks 自省）。
func (s *Scheduler) TaskEnabled(key string) (bool, bool) {
	kind, ok := kindOf(key)
	if !ok {
		return false, false
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return !s.taskDisabled(kind), true
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTraeCheck
	taskTravel
	taskActivity
	taskKeepalive
	taskSchool
	taskCat
	taskGrowth
	taskBalance
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	// 停用判定统一走 taskDisabled（运行期覆盖优先），否则面板刚改的开关要等重启
	// 才在排程上生效——那正是用户抱怨的「任务启停都要重启」。
	s.taskMu.Lock()
	disabled := map[taskKind]bool{}
	for _, k := range []taskKind{taskCheckin, taskTraeCheck, taskTravel, taskActivity,
		taskKeepalive, taskSchool, taskCat, taskGrowth, taskBalance} {
		disabled[k] = s.taskDisabled(k)
	}
	s.taskMu.Unlock()
	var slots []slot
	if !disabled[taskCheckin] {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !disabled[taskTraeCheck] {
		slots = append(slots, slot{nextFire(now, s.cfg.TraeCheckHours), taskTraeCheck})
	}
	if !disabled[taskTravel] {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !disabled[taskActivity] {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !disabled[taskKeepalive] {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if !disabled[taskSchool] {
		slots = append(slots, slot{nextFire(now, s.cfg.SchoolHours), taskSchool})
	}
	if !disabled[taskCat] {
		slots = append(slots, slot{nextFire(now, s.cfg.CatHours), taskCat})
	}
	if !disabled[taskGrowth] {
		slots = append(slots, slot{nextFire(now, s.cfg.GrowthHours), taskGrowth})
	}
	if !disabled[taskBalance] {
		slots = append(slots, slot{nextFire(now, s.cfg.BalanceHours), taskBalance})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	s.taskMu.Lock()
	if s.lifecycle == nil || s.cancel == nil {
		base := s.lifecycle
		if base == nil {
			base = context.Background()
		}
		// #nosec G118 -- 同上：懒初始化的 cancel 同样由 Run 的 defer 收尾
		s.lifecycle, s.cancel = context.WithCancel(base)
	}
	if s.taskLast == nil {
		s.taskLast = make(map[TaskKey]time.Time)
	}
	if s.taskBusy == nil {
		s.taskBusy = make(map[TaskKey]bool)
	}
	cancel := s.cancel
	s.taskMu.Unlock()
	stopForward := context.AfterFunc(ctx, cancel)
	defer stopForward()
	defer func() {
		cancel()
		s.taskMu.Lock()
		s.stopping = true
		s.taskMu.Unlock()
		s.taskWG.Wait()
	}()
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 全部任务都禁用：不空转，只等退出信号或开关变更信号。
			// 必须也听 s.wake：面板把某个任务重新启用后，这里要能醒过来重算时点，
			// 否则进程会一直睡到退出——「启用」点了等于没点。
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			// 开关被改过（SetTaskEnabled）：丢掉旧时点，立刻按新值重算。
			// 不重算的话「刚启用的任务」要等旧 timer 到点才跑——用户会以为没生效。
			timer.Stop()
			continue
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。脚本类（school/cat）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	if !s.beginTask(k) {
		// 该任务已在执行（多为管理面板手动触发撞上定时点）：跳过本轮，不重复打上游。
		log.Printf("scheduler: %s 正在执行，跳过本次定时触发", k.key())
		return
	}
	defer s.endTask(k)
	s.runTask(ctx, k)
}

// runTask 串行执行单类任务：全局互斥 + 日志归集。
// 定时入口（dispatch）与手动触发（TriggerTask）都走这里，保证同一时刻只有一类任务在跑。
func (s *Scheduler) runTask(ctx context.Context, k taskKind) {
	if ctx.Err() != nil {
		return
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	key := k.key()
	taskSink.begin(key)
	defer taskSink.end()

	// 统一的起止标记：有些任务成功时本来一行都不打（保活就是），
	// 面板上会显示成"0 行"，看不出到底跑没跑。有了这对标记，任何任务
	// 都至少能回答"刚才那次执行了没有、花了多久"。
	// defer 注册顺序：先 end 后 marker，LIFO 下 marker 先跑，保证结束行也入环。
	start := time.Now()
	log.Printf("%s: 开始执行", key)
	defer func() {
		log.Printf("%s: 执行结束（耗时 %.1fs）", key, time.Since(start).Seconds())
	}()
	s.runKind(ctx, k)
}

// runKind 执行单类任务，本身不含互斥——互斥由调用方经 beginTask/endTask 负责，
// 定时入口（dispatch）与手动触发（TriggerTask）共用同一把锁。
func (s *Scheduler) runKind(ctx context.Context, k taskKind) {
	switch k {
	case taskCheckin:
		s.runCheckin(ctx)
	case taskTraeCheck:
		s.runTraeCheck(ctx)
	case taskTravel:
		s.runTravel(ctx)
	case taskActivity:
		s.runActivity(ctx)
	case taskKeepalive:
		s.runKeepalive(ctx)
	case taskSchool:
		s.runSchool(ctx)
	case taskCat:
		s.runCat(ctx)
	case taskGrowth:
		s.runGrowthCenter(ctx)
	case taskBalance:
		s.runBalance(ctx)
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
func (s *Scheduler) RunCheckinNow() {
	s.runCheckin(context.Background())
}

func (s *Scheduler) runCheckin(ctx context.Context) {
	if _, err := s.checkinAll(ctx); err != nil {
		log.Printf("定时签到已跳过：%v", err)
	}
}

// ── Trae 签到（独立站点，独立排程）────────────────────────────────────────
//
// Trae 与 WorkBuddy 是**两个不同的站点**：Trae 的签到与积分走 api.trae.cn 的
// UG / pay API，WorkBuddy 走 codebuddy.cn 的 billing/meter。端点、鉴权头族、
// 风控规则都不同，所以拆成两个任务、两套排程，各自可开关。
//
// 9074（"当前参与用户太多"）是**设备维度**的限流。实测（2026-10-11）：同一账号
// 同一 JWT，第 0 代设备号返回 9074，紧接着换新派生的设备号**立刻签到成功**。
// 所以正确处理是「轮换设备号重试一次」，参考实现 autumnsentiment/Trae2api-cn
// 的做法完全一致（见其 rotate_checkin_device_id）。
//
// 我最初把这个信号误判成账号维度、并做成「当天不再重试」——那是错的：实测四个
// Trae 账号在换设备号后当天全部签上了，包括被 9074 卡了一整天的两个。设备代数
// 记在内存（traeDeviceGen），重启即回第 0 代（无害：新设备号只是换个设备身份）。

// runTraeCheck 定时入口：撞车时记日志跳过，不重复打上游。
func (s *Scheduler) runTraeCheck(ctx context.Context) {
	if _, err := s.traeCheckAll(ctx); err != nil {
		log.Printf("定时 Trae 签到已跳过：%v", err)
	}
}

// TraeCheckAll 供管理面板手动触发：逐账号结果由 traeCheckAll 记日志。
func (s *Scheduler) TraeCheckAll() ([]CheckinOutcome, error) {
	return s.traeCheckAll(context.Background())
}

// traeCheckAll 只处理 trae 域的账号：签到 → 查余额 → 解冻。
//
// 与 checkinAll 共用 checkinMu：两个任务虽然站点不同，但都是「签到类」写操作，
// 手动同时触发两个不该并发打上游（各自打各自的站点，但日志与节流口径一致更简单）。
func (s *Scheduler) traeCheckAll(ctx context.Context) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		// 只处理 trae 域；其它域的账号本任务不碰（它们归 TaskKeyCheckin）。
		if a == nil || a.Realm() != trae.Realm {
			continue
		}
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		if a.Snapshot().RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// 仍在 9074 退避窗口内：本轮跳过，别去撞上游（撞了只会延长窗口）。
		if d, waiting := s.traeInBackoff(st.UID); waiting {
			oc.Status, oc.Detail = CheckinSkipped,
				fmt.Sprintf("trae: 9074 退避中，还需 %s", d.Round(time.Second))
			skipN++
			out = append(out, oc)
			continue
		}
		// 本账号当前用的设备号代数（9074 后递增，见下）。
		gen := s.traeDeviceGeneration(st.UID)
		// 与通用签到一致：停机跨过 token 有效期时先补一次刷新。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("trae 签到 %s 刷新凭据：%v", logfmt.UID8(st.UID), err)
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else if err := a.SaveAtomic(); err != nil {
				log.Printf("trae 签到 %s 落盘凭据：%v", logfmt.UID8(st.UID), err)
			}
		}

		traeStatus, traeErr := trae.CheckinStatusOf(ctx, a)
		switch {
		case traeErr != nil:
			oc.Status, oc.Detail = CheckinFail, "trae: "+traeErr.Error()
			failN++
			log.Printf("trae 签到 %s 查状态失败：%v", logfmt.UID8(st.UID), traeErr)
		case !traeStatus.Enabled:
			oc.Status, oc.Detail = CheckinSkipped, "trae: checkin disabled"
			skipN++
		case traeStatus.CheckedIn:
			oc.Status = CheckinAlready
			alreadyN++
		default:
			// 全局节流：距上次 claim 不满 traeClaimGap 就先等。各账号的尝试因此
			// 分散到不同的上游窗口，而不是一起烧在同一个关闭窗口里。
			if werr := s.traeWaitClaimGap(ctx); werr != nil {
				return out, werr
			}
			claimed, claimErr := trae.ClaimCheckin(ctx, a, gen)
			s.traeNoteClaim()
			// 9074 后换一个新派生的设备号重试一次。注意重试**同样受上面的节流**，
			// 所以这里先等满间隔——紧挨着重发只会落在同一个关闭窗口里（实测
			// 01:39 那两次 gen0/gen1 相隔 1 秒，双双 9074）。
			if trae.IsRateLimited(claimErr) {
				if werr := s.traeWaitClaimGap(ctx); werr != nil {
					return out, werr
				}
				gen++
				log.Printf("trae 签到 %s：设备号第 %d 代遇 9074 限流，换到第 %d 代重试",
					logfmt.UID8(st.UID), gen-1, gen)
				claimed, claimErr = trae.ClaimCheckin(ctx, a, gen)
				s.traeNoteClaim()
				// 无论新设备号成没成，都把代数记下来：旧代数已经确认被限流，
				// 下次还从它开始只会白撞一次（实测 2026-10-12 00:00 那两个号
				// 连 gen0→gen1 都吃 9074，若只在成功时记住，每轮都要重走一遍 gen0）。
				s.noteTraeDeviceGeneration(st.UID, gen)
			}
			if claimErr != nil {
				oc.Status, oc.Detail = CheckinFail, "trae: "+claimErr.Error()
				failN++
				// 轮换后仍 9074（上游高峰期的容量信号，换设备号也绕不过）：
				// 记一层有界退避，避免本进程后续轮次继续撞。
				if trae.IsRateLimited(claimErr) {
					d := s.noteTraeBackoff(st.UID)
					oc.Detail += fmt.Sprintf("（退避 %s）", d.Round(time.Second))
				}
				log.Printf("trae 签到 %s 领奖失败：%v", logfmt.UID8(st.UID), claimErr)
			} else {
				oc.Status = CheckinOK
				gained := claimed.Credits + claimed.ExtraCredits
				if gained > 0 {
					oc.Detail = fmt.Sprintf("trae: +%.0f credits", gained)
				}
				s.clearTraeBackoff(st.UID)
				okN++
			}
		}
		// 顺手读余额（纯读接口，与签到是两件事）。
		if credits, cerr := trae.FetchCredits(ctx, a); cerr == nil {
			remain := int64(credits.Remaining)
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
			s.cfg.Pool.SetCredits(st.UID, remain)
			oc.Credits = &remain
		} else {
			log.Printf("查余额 %s（trae）：%v", logfmt.UID8(st.UID), cerr)
		}
		out = append(out, oc)
	}
	log.Printf("trae 签到完成：共 %d，成功 %d，已签 %d，失败 %d，跳过 %d",
		len(out), okN, alreadyN, failN, skipN)
	return out, nil
}

// traeBackoffEntry 单账号的 9074 退避状态。
type traeBackoffEntry struct {
	until  time.Time // 下次允许尝试的时刻
	streak int       // 连续命中次数（决定退避翻几倍）
}

const (
	// traeClaimGap 两次 claim 调用之间的最小间隔（全局，跨账号）。
	//
	// 9074 是上游**容量信号**（"当前参与用户太多"），会随上游负载开合。连打不会
	// 「造成」9074，但会把所有账号的尝试全部烧在**同一个关闭的窗口**里：实测
	// 2026-10-12 01:39 四个号 5.4 秒跑完，8 次调用全 9074（含换设备号后的重试）；
	// 而 01:41 把同样的调用分散在 90 秒里，三次全 success。
	//
	// 分散开的收益不是「更礼貌」，而是**各自撞不同的窗口**——窗口开着的那次就成了。
	// 参考实现（autumnsentiment/Trae2api-cn）同样有 CHECKIN_INTERVAL 全局间隔，
	// 默认 60 秒。
	traeClaimGap = 60 * time.Second
	// traeBackoffBase 首次 9074 后的冷却基数。
	traeBackoffBase = 60 * time.Second
	// traeBackoffExpCap 退避指数上限：60s × 2^3 = 8 分钟就够——
	// 上游限流是短窗口的，退避太久会白白错过当天可签的时机。
	traeBackoffExpCap = 3
)

// traeWaitClaimGap 等到距上次 claim 满 traeClaimGap，返回是否因取消而中断。
//
// 全局节流：不是按账号，而是限制**整个进程**的发 claim 速率。理由见 traeClaimGap。
func (s *Scheduler) traeWaitClaimGap(ctx context.Context) error {
	s.mu.Lock()
	wait := time.Until(s.traeLastClaim.Add(traeClaimGap))
	s.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// traeNoteClaim 记下刚刚发出了一次 claim。
func (s *Scheduler) traeNoteClaim() {
	s.mu.Lock()
	s.traeLastClaim = time.Now()
	s.mu.Unlock()
}

// traeInBackoff 报告该账号是否仍在 9074 退避窗口内。
func (s *Scheduler) traeInBackoff(uid string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.traeBackoff[uid]
	if !ok {
		return 0, false
	}
	if d := time.Until(e.until); d > 0 {
		return d, true
	}
	return 0, false
}

// noteTraeBackoff 记一次 9074，返回本轮应冷却多久。
func (s *Scheduler) noteTraeBackoff(uid string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.traeBackoff == nil {
		s.traeBackoff = map[string]traeBackoffEntry{}
	}
	e := s.traeBackoff[uid]
	e.streak++
	exp := e.streak - 1
	if exp > traeBackoffExpCap {
		exp = traeBackoffExpCap
	}
	d := traeBackoffBase << exp
	e.until = time.Now().Add(d)
	s.traeBackoff[uid] = e
	return d
}

// clearTraeBackoff 签到成功后清退避（下一次从基数重新开始）。
func (s *Scheduler) clearTraeBackoff(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.traeBackoff, uid)
}

// traeDeviceGeneration / noteTraeDeviceGeneration：Trae 签到的设备号代数。
//
// 9074 是设备维度的限流，换一个**新派生**的设备号即可立刻签上。代数记在内存：
// 命中 9074 → 代数 +1 → 用新设备号重试；成功后把代数留住，后续轮次直接用新的，
// 免得每次都被旧的限流设备号挡一道。进程重启回第 0 代（无害：设备身份换一个而已，
// 上游对签到是按设备去重，第 0 代本身也能用——只是可能还在限流窗口里）。
func (s *Scheduler) traeDeviceGeneration(uid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.traeDeviceGen[uid]
}

func (s *Scheduler) noteTraeDeviceGeneration(uid string, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.traeDeviceGen == nil {
		s.traeDeviceGen = map[string]int{}
	}
	s.traeDeviceGen[uid] = gen
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	return s.checkinAll(context.Background())
}

func (s *Scheduler) checkinAll(ctx context.Context) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，**不发起签到调用**（避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		//
		// 2026-10-10 修正：跳过签到调用，但**仍要读余额**（isGlobal 标记后继续往下走，
		// 只是不发 DailyCheckin）。原先这里直接 continue，导致 global 账号的
		// credits_expiring 永远为空——而「付费模型优先消耗快过期积分」正依赖这个字段，
		// 于是该特性对 global 号（28 个里 23 个）完全失效。
		// 余额查询是纯读接口，与签到是两件事：面板的 ./credit 一直对全部 28 个号查余额
		// 且工作正常，说明 global 号读余额不触发风控；被跳过的是签到（写操作）。
		// trae 域走完全不同的端点族：签到与余额查询都必须跳过（拿 Trae 的
		// Cloud-IDE-JWT 打 codebuddy.cn 只会 401，且每轮刷一遍日志）。
		// isWorkBuddy 为 false 时两件事都不做——它不是「国际版不签到但读余额」
		// 那种语义，而是「根本不是这套端点」。
		isWorkBuddy := a.IsWorkBuddy()
		// Trae 号整条跳过，**在刷新之前**：它不是 WorkBuddy 的端点族，签到与余额
		// 都不适用，而且 Trae 的 refreshToken 是一次性轮换的——为了一个不存在的
		// 签到去刷它，只会白白消耗凭据。它们归独立的 TaskKeyTraeCheck。
		if !isWorkBuddy {
			oc.Status, oc.Detail = CheckinSkipped, a.Realm()+" (separate task)"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("签到 %s 刷新凭据：%v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				s.cfg.Pool.ClearSessionDead(st.UID)
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("签到 %s 落盘凭据：%v", logfmt.UID8(st.UID), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// global 号没有签到体系：跳过签到调用（写操作，会触发风控），但下面的余额查询照做。
		// trae 号在更上面已整条 continue（端点族不同，签到与余额都不适用）。
		if isWorkBuddy && a.Realm() != auth.RealmGlobal {
			if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
				if upstream.IsAlreadyCheckin(err) {
					// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
					// 出现一整段 400 报文、被误读成签到失败。
					oc.Status = CheckinAlready
				} else {
					oc.Status = CheckinFail
					oc.Detail = err.Error()
					log.Printf("签到 %s 失败：%v", logfmt.UID8(st.UID), err)
				}
			} else {
				oc.Status = CheckinOK
			}
		} else {
			// global 号：本次只做余额查询，签到状态如实记为「跳过」。
			oc.Status = CheckinSkipped
			skipN++
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗（issue:积分过期）。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		if err := ctx.Err(); err != nil {
			return out, err
		}
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("查余额 %s 失败：%v", logfmt.UID8(st.UID), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		case CheckinSkipped:
			// global 号：签到跳过但余额已更新（skipN 已在上面计过，这里不重复计）。
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("签到完成：共 %d，成功 %d，已签 %d，失败 %d，跳过 %d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// ── 余额刷新（独立任务，2026-10-14）────────────────────────────────────────
//
// 把「查余额 → 写池」从签到任务里解耦出来。原来这件事只发生在 checkinAll 内部，
// 于是关掉签到（多客户端共用账号的用户必须这么做）就顺带把池里的 credits 与
// credits_expiring 也冻住了，而「付费模型优先消耗快过期积分」正依赖后者
// （见 pool/pick.go 的 paid_expiring 硬过滤）——表现为该特性静默失效：
// 面板有数据（面板直查上游），选号却完全没有快过期偏好。
//
// 与签到的分工：
//   - 签到（taskCheckin）：写操作（DailyCheckin），按 checkin_hours 排程；
//   - 余额（taskBalance）：**纯读**（UserResourceDetailed），按 balance_hours 排程。
//
// 两者都保留自己的余额查询，是刻意的：签到后立刻查一次能让刚签到的积分马上
// 参与选号，而独立任务负责在「没签到」时也保持数据新鲜。重复查询的代价是每个
// 账号多一次只读请求，可忽略；漏查的代价是选号偏好静默失效，不可接受。

// RunBalanceNow 定时触发的立即余额刷新。
func (s *Scheduler) RunBalanceNow() {
	s.runBalance(context.Background())
}

func (s *Scheduler) runBalance(ctx context.Context) {
	if _, err := s.balanceAll(ctx); err != nil {
		log.Printf("定时余额刷新已跳过：%v", err)
	}
}

// BalanceAll 供管理面板手动触发。
func (s *Scheduler) BalanceAll() ([]CheckinOutcome, error) {
	return s.balanceAll(context.Background())
}

// balanceAll 逐账号查余额并写进池子（不签到、不刷新 token 之外无任何写操作）。
//
// 与 checkinAll 共用 checkinMu：两者都要改池子里的 credits，且都打同一族上游接口，
// 并发跑没有收益、只会让日志与限速口径变复杂。
func (s *Scheduler) balanceAll(ctx context.Context) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, failN, skipN int
	for _, st := range statuses {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// 只处理 WorkBuddy 端点族（cn/global）。trae 走完全不同的 API，余额查询
		// 不适用——拿 Trae 的 JWT 打 codebuddy.cn 只会 401，还会白刷一遍日志。
		if !a.IsWorkBuddy() {
			oc.Status, oc.Detail = CheckinSkipped, a.Realm()+" (separate task)"
			skipN++
			out = append(out, oc)
			continue
		}
		// token 过期时先补一次刷新，否则余额查询必然 401。这里与 checkinAll 的
		// 处理一致：刷新失败但 token 仍未过期就继续（接口抖动不该让本可成功的
		// 查询被跳过）；真正过期才判失败。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("余额刷新 %s 刷新凭据：%v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: balance %s: 连续 %d 次 12153 session dead — 禁用",
							logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				s.cfg.Pool.ClearSessionDead(st.UID)
				a.BackfillRealm()
				if err := a.SaveAtomic(); err != nil {
					log.Printf("余额刷新 %s 落盘凭据：%v", logfmt.UID8(st.UID), err)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("查余额 %s 失败：%v", logfmt.UID8(st.UID), err)
			oc.Status, oc.Detail = CheckinFail, "resource: "+err.Error()
			failN++
			out = append(out, oc)
			continue
		}
		// ReenableIfCredits 会在余额恢复时解冻冷却中的账号——这是签到任务里
		// 「余额查询解冻」的同一件事，独立任务也必须做，否则关了签到就再没有
		// 任何路径能把账号解冻回来。
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Status = CheckinOK
		oc.Credits = &remain
		okN++
		out = append(out, oc)
	}
	log.Printf("余额刷新完成：共 %d，成功 %d，失败 %d，跳过 %d", len(statuses), okN, failN, skipN)
	return out, nil
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().AccessToken == "" {
			continue
		}
		// 活动上报走 WorkBuddy 的 billing 端点。非 WorkBuddy 域（trae）没有这套端点，
		// 拿它的 JWT 去打只会 401 并刷日志——原先这里没有任何域判断，trae 账号会被
		// 逐条送去撞墙（2026-10-11 接入 trae 时发现）。
		if !a.IsWorkBuddy() {
			continue
		}
		// global 门控（2026-10-14）：国际版不做任何任务。此前这里放行 global
		// （PR #45 实测国际版 /v2/report 返回 code=0），但用户明确要求删掉国际版
		// 任务——23 个 global 号每轮上报 5 条，纯属给上游送无意义的写流量。
		if a.Realm() == auth.RealmGlobal {
			continue
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			if ctx.Err() != nil {
				return
			}
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("活跃上报 %s：第 %d/%d 条失败：%v", logfmt.UID8(a.UID), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("活跃上报 %s：第 %d/%d 条成功", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔，避免秒发风控；取消时立即放弃本号剩余条数。
				if !sleepCtx(ctx, activityReportGap) {
					return
				}
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		if ctx.Err() != nil {
			return
		}
		s.checkActivityStreak(a)
		if ctx.Err() != nil {
			return
		}
		s.travelAdoptForce(ctx, a)
		// 连登兑换/抽奖已迁到独立的成长中心任务（taskGrowth，见 growth_center.go）：
		// 那是完整七段流水线，本任务只负责「上报 → 自检 → 领养前置」。
		// 原来这里内联调用 claimGrowthRewardsContext，与流水线重复打同一批接口。
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: 活跃上报 %s 已成功，但回读连登天数失败：%v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: 活跃上报 %s 返回成功但连登天数仍为 0（疑似被服务端静默丢弃）", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("活跃上报 %s：连登 %d 天", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	s.runKeepalive(context.Background())
}

func (s *Scheduler) runKeepalive(ctx context.Context) {
	// 成功路径本来完全静默（只在失败时打 WARN），面板上会是一片空白。
	// 统计后补一行汇总，至少能看出"刷了几个号、失败几个"。
	okCnt, failCnt, skipCnt, repairCnt, reviveCnt := 0, 0, 0, 0, 0
	defer func() {
		log.Printf("keepalive: 刷新成功 %d，失败 %d，跳过 %d，补注册地 %d，自动复活 %d",
			okCnt, failCnt, skipCnt, repairCnt, reviveCnt)
	}()
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.Snapshot().RefreshToken == "" {
			skipCnt++
			continue
		}
		if st.Disabled {
			// 内容审核标记的号不能靠 refresh 复活：它的令牌本来就是好的（拒绝发生在
			// 模型调用层），refresh 成功不代表上游标记已解除。自动复活只会形成
			// 「复活 → 再被拒 → 再停用」的循环，每轮白烧上游请求并拖慢客户端
			// （2026-10-07 实测：这类号 48h 内 0 成功、数百次拒绝）。
			// 只有人工重登后经 /accounts/revive 才回到池中。
			if st.DisabledReason == pool.ReviewFailReason() {
				skipCnt++
				continue
			}
			// 停用号不再永久躺平：refresh 成功即证明账号在鉴权层还活着（P0-1 实测：13 个
			// 被误停用的号 refresh 全部成功，是历史误判的受害者），据此自动复活回到池中。
			// 真正被封的号复活后会因连续两次账号故障再次被禁用——代价只是两次很快的失败。
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				skipCnt++
				continue
			}
			s.cfg.Pool.ReviveDisabled(st.UID)
			reviveCnt++
			log.Printf("INFO: keepalive %s: 处于停用但 refresh 成功 — 自动复活并回到池中", logfmt.UID8(st.UID))
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			failCnt++
			log.Printf("token 保活 %s 刷新失败：%v", logfmt.UID8(st.UID), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("token 保活 %s 落盘凭据：%v", logfmt.UID8(st.UID), err)
		}
		okCnt++
		// 国际版注册地补全：账号没在官方登录流程里确认国家/地区时，chat 会被上游以
		// 14017（trial not activated）永久拒绝。这里每天顺手补一次（幂等：已登记号
		// 只多一次 register 调用，无副作用），让存量坏号不必等到被轮转选中才自愈。
		// 失败只记 WARN，不影响 token 刷新结果。
		if a.Realm() == auth.RealmGlobal {
			area, rerr := s.cfg.Upstream.CompleteRegion(a)
			switch {
			case rerr != nil:
				log.Printf("WARN: token 保活 %s 补注册地失败：%v", logfmt.UID8(st.UID), rerr)
			case area.IOS2 != "":
				repairCnt++
				log.Printf("INFO: token 保活 %s 已补注册地：%s（%s）", logfmt.UID8(st.UID), area.IOS2, area.EnName)
			}
		}
	}
}
