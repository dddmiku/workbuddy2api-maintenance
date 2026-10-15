// ═══ 更新日志 ═══
// 2026-09-18：手动任务复用可取消生命周期，并计入关停等待，停止后拒绝新任务。
// 2026-09-15: 新增。排程任务自省 + 手动触发：供 /tasks 端点与账户管理面板读取各类任务的
//   小时 / 开关 / 下次运行 / 上次完成 / 是否在跑，并可按 key 手动触发任意一类。
//   互斥复用 dispatch 的 beginTask/endTask，手动触发与定时点不会重复打上游。

package scheduler

import (
	"errors"
	"log"
	"time"
)

// TaskKey 九类排程任务的稳定对外标识。这是契约：/tasks 端点与管理面板按它寻址，
// 不要随内部 taskKind 的枚举顺序变动（内部顺序属于实现细节，可直接重排）。
type TaskKey string

const (
	TaskKeyCheckin   TaskKey = "checkin"    // 签到 + 余额查询解冻
	TaskKeyTraeCheck TaskKey = "trae_check" // Trae 域签到（独立站点，独立排程）
	TaskKeyTravel    TaskKey = "travel"     // 猫猫旅行：领养 / 派出 / 领奖
	TaskKeyActivity  TaskKey = "activity"   // 活跃上报：连登天数 + 领猫前置
	TaskKeyKeepalive TaskKey = "keepalive"  // token 保活
	TaskKeySchool    TaskKey = "school"     // 开学季任务
	TaskKeyCat       TaskKey = "cat"        // 夜猫子任务
	TaskKeyGrowth    TaskKey = "growth"     // 成长中心流水线（旅行/任务奖/补登/兑换/抽奖/Buddy盲盒）
	TaskKeyBalance   TaskKey = "balance"    // 余额刷新（纯读；快过期优先消耗的数据来源）
)

// ErrUnknownTask 未知任务 key；ErrTaskBusy 该任务当前正在执行。
var (
	ErrUnknownTask      = errors.New("unknown task key")
	ErrTaskBusy         = errors.New("task is already running")
	ErrSchedulerStopped = errors.New("scheduler is stopping")
)

// TaskInfo 单类任务的对外快照。时间字段用指针：JSON 里 null 表达"从未运行过 / 已禁用无排程"，
// 与"1970 年"这类哨兵值区分开，前端不必猜。
type TaskInfo struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
	// Realms 该任务作用于哪些账号域。面板据此**按区域分类**展示任务——
	// 2026-10-11 用户要求「任务点按照实际账号区域分类」。
	//
	// 值必须与任务实现里真实作用的域一致，不能按名字猜：
	//   - checkin 现在同时覆盖 cn/global（WorkBuddy 端点）与 trae（Trae 自己的端点），
	//     所以是三个域；其余任务只走 WorkBuddy 端点，trae 账号在调度里被跳过。
	// 改任务实现时**必须同步改这里**，否则面板会把任务归错组。
	Realms  []string   `json:"realms"`
	Hours   []int      `json:"hours"`
	Enabled bool       `json:"enabled"`
	NextRun *time.Time `json:"next_run"`
	LastRun *time.Time `json:"last_run"`
	Running bool       `json:"running"`
}

// key 内部 taskKind → 对外 TaskKey。返回空串表示未登记的种类（不该出现）。
func (k taskKind) key() TaskKey {
	switch k {
	case taskCheckin:
		return TaskKeyCheckin
	case taskTraeCheck:
		return TaskKeyTraeCheck
	case taskTravel:
		return TaskKeyTravel
	case taskActivity:
		return TaskKeyActivity
	case taskKeepalive:
		return TaskKeyKeepalive
	case taskSchool:
		return TaskKeySchool
	case taskCat:
		return TaskKeyCat
	case taskGrowth:
		return TaskKeyGrowth
	case taskBalance:
		return TaskKeyBalance
	}
	return ""
}

// kindOf 对外 TaskKey → 内部 taskKind。
func kindOf(key string) (taskKind, bool) {
	switch TaskKey(key) {
	case TaskKeyCheckin:
		return taskCheckin, true
	case TaskKeyTraeCheck:
		return taskTraeCheck, true
	case TaskKeyTravel:
		return taskTravel, true
	case TaskKeyActivity:
		return taskActivity, true
	case TaskKeyKeepalive:
		return taskKeepalive, true
	case TaskKeySchool:
		return taskSchool, true
	case TaskKeyCat:
		return taskCat, true
	case TaskKeyGrowth:
		return taskGrowth, true
	case TaskKeyBalance:
		return taskBalance, true
	}
	return 0, false
}

// describe 任务的展示名与说明。与 README「定时积分任务」一节逐条对应，
// 面板直接透出，避免同一套文案在两处各写一遍后漂移。
func describe(k TaskKey) (string, string) {
	switch k {
	case TaskKeyCheckin:
		return "签到（WorkBuddy）", "WorkBuddy 两域每日签到 + 余额查询；余额恢复后自动解冻冷却中的账号"
	case TaskKeyTraeCheck:
		return "签到（Trae）", "Trae 站点独立签到；上游 9074 是设备维度限流，换设备号重试一次"
	case TaskKeyActivity:
		return "活跃上报", "对话事件连发上报，点亮连登天数、解锁领养前置"
	case TaskKeyTravel:
		return "猫猫旅行", "独立排程：领养 / 派出 / 领奖闭环推进"
	case TaskKeyKeepalive:
		return "token 保活", "全账号刷新 token；session 失效连续 3 次才禁用"
	case TaskKeySchool:
		return "开学季任务", "任务点亮 + claim + 自动抽空抽奖余额；活动下线时自动跳过"
	case TaskKeyCat:
		return "夜猫子任务", "夜猫窗口（23:00–08:00 CST）内补一次 black_cat 任务"
	case TaskKeyGrowth:
		return "成长中心", "一轮跑完七段：领旅行礼物/派 Buddy → 任务接单与领奖 → 补登卡自动补 → 连登兑换 → 盲盒抽奖 → 能量开 Buddy 盲盒 → 能量与连签展示"
	case TaskKeyBalance:
		return "余额刷新", "只读查询各账号积分余额与快过期分桶，驱动「优先消耗快过期积分」选号；关掉签到也建议保留本任务"
	}
	return string(k), ""
}

// realmsOf 返回该任务实际作用的账号域（面板按此分组展示）。
//
// 这份表是**任务实现的事实描述**，不是愿望：写错会让面板把任务归到不存在的域下。
//
// 2026-10-14 重大变更：**国际版（global）不再参与任何任务**。用户明确要求删掉
// 国际版任务（「国际版没有任务」）——global 的积分体系与 CN 是两套账，端点虽同构
// 但大量接口对 global 新号返回 500，而这批号占池子大头（23/34）。现在各任务的
// realm 门控按 realm 精确排除 global（见 travel.go / scheduler.go 的 RealmGlobal 判断）。
//
// 唯一的例外是 checkin 与 balance：global 号仍查余额（纯读，驱动「优先消耗快过期
// 积分」选号，不触发风控），但不发签到/上报等任何写调用。所以这两个任务的 Realms
// 标 cn+global，其余任务只标 cn。
func realmsOf(k TaskKey) []string {
	switch k {
	case TaskKeyCheckin:
		// cn 走完整签到 + 余额；global 只查余额（实现里 RealmGlobal 分支跳过签到）。
		return []string{"cn", "global"}
	case TaskKeyTraeCheck:
		return []string{"trae"}
	case TaskKeyBalance:
		// 纯读余额查询，cn 与 global 都跑（trae 在实现里整条跳过）。
		return []string{"cn", "global"}
	}
	// 其余任务只服务 CN：global 在实现里被精确排除。
	return []string{"cn"}
}

// scheduleOf 单类任务的排程小时与开关状态（来自 Config，即 config.json 的 schedule 段）。
//
// 开关状态**不是**直接读 cfg 的 Disabled 字段：面板改完 config.json 会经
// SetTaskEnabled 写运行期覆盖，而 cfg 里的值只在启动时抄过一次。读 cfg 会让面板
// 刚点的开关在任务页上不翻转（显示旧值），直到重启——2026-10-15 用户反馈的
// 「任务启停都要重启」正是这个。
func (s *Scheduler) scheduleOf(k TaskKey) ([]int, bool) {
	kind, ok := kindOf(string(k))
	if !ok {
		return nil, false
	}
	s.taskMu.Lock()
	enabled := !s.taskDisabled(kind)
	s.taskMu.Unlock()
	switch k {
	case TaskKeyCheckin:
		return s.cfg.CheckinHours, enabled
	case TaskKeyTraeCheck:
		return s.cfg.TraeCheckHours, enabled
	case TaskKeyTravel:
		return s.cfg.TravelHours, enabled
	case TaskKeyActivity:
		return s.cfg.ActivityHours, enabled
	case TaskKeyKeepalive:
		return s.cfg.KeepaliveHours, enabled
	case TaskKeySchool:
		return s.cfg.SchoolHours, enabled
	case TaskKeyCat:
		return s.cfg.CatHours, enabled
	case TaskKeyGrowth:
		return s.cfg.GrowthHours, enabled
	case TaskKeyBalance:
		return s.cfg.BalanceHours, enabled
	}
	return nil, false
}

// TaskSnapshot 返回全部任务的当前快照，顺序固定为
// 签到（WorkBuddy）/ 签到（Trae）/ 猫猫旅行 / 活跃上报 / token 保活 / 开学季 /
// 夜猫子 / 成长中心 / 余额刷新。
//
// 2026-10-14：原 redeem/lottery/makeup 三条独立任务合并为 growth（成长中心）一条
// ——它们本是一条有依赖的流水线，拆开跑既丢顺序又白等半天（详见 growth_center.go）。
func (s *Scheduler) TaskSnapshot() []TaskInfo {
	now := time.Now()
	keys := []TaskKey{
		TaskKeyCheckin, TaskKeyTraeCheck, TaskKeyTravel, TaskKeyActivity,
		TaskKeyKeepalive, TaskKeySchool, TaskKeyCat,
		TaskKeyGrowth, TaskKeyBalance,
	}
	out := make([]TaskInfo, 0, len(keys))
	for _, key := range keys {
		name, detail := describe(key)
		hours, enabled := s.scheduleOf(key)
		info := TaskInfo{
			Key: string(key), Name: name, Detail: detail,
			Hours: hours, Enabled: enabled, Realms: realmsOf(key),
		}
		// 禁用任务没有下个时点（nextWake 同样跳过），置 nil 而不是留一个假的未来时刻。
		if enabled {
			if at := nextFire(now, hours); !at.IsZero() {
				info.NextRun = &at
			}
		}
		s.taskMu.Lock()
		if t, ok := s.taskLast[key]; ok {
			last := t
			info.LastRun = &last
		}
		info.Running = s.taskBusy[key]
		s.taskMu.Unlock()
		// 内存里没有上次时刻（进程刚重启）时，回落到落盘的完成时刻——
		// 否则任务页的「上次运行」在每次部署后都变成空白，而日志却还在
		// （两者取自同一份事实，不该一个有一个没有）。
		if info.LastRun == nil {
			if t := taskSink.lastEnded(key); !t.IsZero() {
				last := t
				info.LastRun = &last
			}
		}
		out = append(out, info)
	}
	return out
}

// beginTask 占用单类任务；已被占用返回 false。定时入口与手动触发共用，保证不并发。
func (s *Scheduler) beginTask(k taskKind) bool {
	key := k.key()
	if key == "" {
		return false
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.stopping || s.lifecycle.Err() != nil || s.taskBusy[key] {
		return false
	}
	s.taskBusy[key] = true
	s.taskWG.Add(1)
	return true
}

// endTask 释放任务并记下完成时刻（供面板显示"上次运行"）。
func (s *Scheduler) endTask(k taskKind) {
	key := k.key()
	if key == "" {
		return
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if !s.taskBusy[key] {
		return
	}
	s.taskBusy[key] = false
	s.taskLast[key] = time.Now()
	taskSink.markEnded(key)
	s.taskWG.Done()
}

// TriggerTask 手动触发单类任务。任务在后台 goroutine 里跑，调用立即返回。
// 正在执行时返回 ErrTaskBusy——不排队：连点几次就往上堆一串重复的上游调用，
// 对签到这个"同一时刻只许一次"的接口尤其危险。
func (s *Scheduler) TriggerTask(key string) error {
	k, ok := kindOf(key)
	if !ok {
		return ErrUnknownTask
	}
	if s.lifecycle.Err() != nil {
		return ErrSchedulerStopped
	}
	if !s.beginTask(k) {
		return ErrTaskBusy
	}
	go func() {
		defer s.endTask(k)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERR: scheduler: 手动触发 %s panic: %v", key, r)
			}
		}()
		log.Printf("scheduler: 手动触发 %s", key)
		s.runTask(s.lifecycle, k)
	}()
	return nil
}
