// ═══ 更新日志 ═══
// 2026-10-14：新增。成长中心完整流水线，语义移植自 88lin/workbuddy-auto-signin
// （MIT，https://github.com/88lin/workbuddy-auto-signin 的 run_growth）。
//
// 为什么是原生 Go 而不是继续用 Python 脚本：原来成长中心拆在 scripts/growth_center.py
// （只做 redeem/lottery/makeup）与 scripts/task_runner.py（只做任务点亮）两个脚本里，
// 88lin 脚本验证过的这一段——**任务领奖（accept + claim）**、**补登卡自动补**、
// **Buddy 能量盲盒**——网关侧完全没有。整条链路一次跑完才是官方客户端的形态：
// 领任务奖送的抽奖次数当场就能抽、兑换送的补登卡当场就能补、能量攒够当场就开盲盒。
//
// 保留 Python 脚本的部分：task_runner.py 的**任务点亮**（伪造桌面/web 事件把
// create_canvas/chat_5/template_5 等 16 种任务做成 completed）是 88lin 没有的能力，
// 它只领「真完成」的任务；这段继续由 taskSchool/taskCat 调度走脚本。本文件的
// claimGrowthTasks 只做「accept → 已完成任务的 claim」，不伪造任何事件。
//
// realm 门控：本流水线只服务 CN。国际版（global）**完全不参与**——用户 2026-10-14
// 明确要求删掉国际版任务（「国际版没有任务」）。全球号的积分体系与 CN 是两套账，
// 端点虽同构但 streak 等接口对 global 新号返回 500（实测 sliverkiss），
// 且这批号占了池子的大头（23/34），跑一遍只是徒增上游调用与日志噪音。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// ── 常量与限速 ─────────────────────────────────────────────────────────────

const (
	// makeupMaxPerRun 单轮最多用几张补登卡。88lin 取 1：补登会改变连登天数，
	// 一次补一天足够保住当前连登，且写操作越少越不容易踩上游风控。
	makeupMaxPerRun = 1

	// growthTaskAcceptBatch 接单分批大小。88lin 取 20：别把 body 撑大。
	growthTaskAcceptBatch = 20

	// growthMakeupFloor 补登最早可补日期。活动上线日之前的空格不该补——
	// 88lin 用固定 2026-06-17 兜底（launch_date 缺失时）。
	growthMakeupFloor = "2026-06-17"

)

// growthAccountDelay 账号间限速：与 travel/activity 同口径（800ms）。
// 成长中心每号要打 8–10 个接口，比旅行更重，限速不能更松。测试可置 0。
var growthAccountDelay = 800 * time.Millisecond

// growthTier 连登兑换档位：(tier 标识, summary 状态字段前缀, 展示名, 天数)。
//
// tier 必须传**档位标识字符串**（"7d"/"14d"/"28d"）：88lin 实测传 "starter"/"7"/7
// 分别得到 unknown tier / unknown tier / invalid request，只有 "7d" 会 200。
// 天数只在服务端判 unknown tier 时用于兜底重试（旧写法兼容）。
var growthTiers = []struct {
	Tier    string
	Status  string
	Label   string
	Days    int
}{
	{"7d", "starter", "入门", 7},
	{"14d", "advanced", "进阶", 14},
	{"28d", "legendary", "巅峰", 28},
}

// growthRedeemFallbackDesc 兑换成功的回落文案（服务端没给 *_granted 明细时用）。
// 活动改版时以服务端响应为准，这里只是兜底。
var growthRedeemFallbackDesc = map[string]string{
	"7d":  "+2 能量 +1 补登卡 +1 次抽奖",
	"14d": "+50 积分 +3 能量 +1 补登卡 +1 次抽奖",
	"28d": "+150 积分 +5 能量 +1 补登卡 +1 次抽奖",
}

// ── 流水线结果 ─────────────────────────────────────────────────────────────

// growthOutcome 一轮成长中心的汇总，用于日志与测试断言。
type growthOutcome struct {
	Parts         []string // 人话步骤记录（88lin 的 parts 同款）
	CreditsGained int
	Energy        int
	StreakDays    int
	Successes     int
	Failures      int
	HardFailures  int // 需要关注的失败（5xx / 网络 / 查询失败）
}

// note 追加一条步骤记录。
func (o *growthOutcome) note(format string, args ...any) {
	o.Parts = append(o.Parts, fmt.Sprintf(format, args...))
}

// report 组装一行人话汇报（88lin 的 report 同款语义）。
func (o *growthOutcome) report() string {
	var b strings.Builder
	if len(o.Parts) > 0 {
		b.WriteString(strings.Join(o.Parts, "；"))
	} else if o.Failures > 0 {
		b.WriteString("成长中心各步骤均失败")
	} else {
		b.WriteString("成长中心无可领取项")
	}
	tail := []string{}
	if o.Energy > 0 {
		tail = append(tail, fmt.Sprintf("能量 %d", o.Energy))
	}
	if o.StreakDays > 0 {
		tail = append(tail, fmt.Sprintf("连签 %d 天", o.StreakDays))
	}
	if o.CreditsGained > 0 {
		tail = append(tail, fmt.Sprintf("本次 +共 %d 积分", o.CreditsGained))
	}
	if len(tail) > 0 {
		b.WriteString("（" + strings.Join(tail, "，") + "）")
	}
	return b.String()
}

// RunGrowthNow 立即对池内 CN 账号跑一轮完整成长中心（手动触发入口）。
func (s *Scheduler) RunGrowthNow() {
	s.runGrowthCenter(context.Background())
}

// runGrowthCenter 遍历池内 CN 账号，逐号跑一轮成长中心流水线。
//
// 只服务 CN：global 与 trae 整条跳过（见文件头注释）。账号间限速 growthAccountDelay，
// ctx 取消立即退出（剩余账号下轮再跑）。
func (s *Scheduler) runGrowthCenter(ctx context.Context) {
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
		// CN 门控：成长中心只服务 CN 号。global（国际版无任务体系）与 trae
		// （端点族完全不同）都跳过。
		if !a.IsWorkBuddy() || a.Realm() == auth.RealmGlobal {
			continue
		}
		if !first {
			if !sleepCtx(ctx, growthAccountDelay) {
				return
			}
		}
		first = false
		s.growthOne(ctx, a)
	}
}

// growthOne 单账号一轮完整成长中心：88lin run_growth 的七段。
//
// 段序（与 88lin 一致，顺序有依赖）：
//  1. Buddy 旅行：领礼物 / 派出发
//  2. 任务领奖（accept → claim）：送的抽奖次数/能量后面立刻能用
//  3. 补登卡：先补，兑换才能拿到最新解锁状态
//  4. 连登兑换
//  5. 盲盒抽奖
//  6. Buddy 能量盲盒
//  7. 能量 & 连签展示值（纯读，不计失败）
//
// 认证/权限拒绝（401/403）立即中止本轮——同一凭据继续打只会连撞。
func (s *Scheduler) growthOne(ctx context.Context, a *auth.Auth) {
	uid8 := logfmt.UID8(a.UID)
	o := &growthOutcome{}
	// streak 快照复用：补登会改连登天数（stale），此时第 7 段必须重取。
	var streakSnap *upstream.GrowthStreakFull
	streakStale := false

	// 硬失败判定：5xx / 网络层错误需要关注；4xx 业务态不算。
	isHard := func(err error) bool {
		var ue *upstream.Error
		if !errors.As(err, &ue) {
			return err != nil // 网络层/解析层错误按硬失败
		}
		return ue.Status >= 500
	}

	// ── 1. Buddy 旅行：领礼物 / 派出发 ──────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	if authStop := s.growthTravel(ctx, a, o, isHard); authStop {
		log.Printf("growth %s: 认证拒绝，中止本轮：%s", uid8, o.report())
		return
	}

	// ── 2. 任务领奖 ────────────────────────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	if authStop := s.claimGrowthTasks(ctx, a, o, isHard); authStop {
		log.Printf("growth %s: 认证拒绝，中止本轮：%s", uid8, o.report())
		return
	}

	// ── 3. 补登卡 ──────────────────────────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	s.growthMakeup(ctx, a, o, isHard, &streakSnap, &streakStale)

	// ── 4. 连登兑换 ────────────────────────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	if authStop := s.growthRedeem(ctx, a, o, isHard); authStop {
		log.Printf("growth %s: 认证拒绝，中止本轮：%s", uid8, o.report())
		return
	}

	// ── 5. 盲盒抽奖 ────────────────────────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	if authStop := s.growthLottery(ctx, a, o, isHard); authStop {
		log.Printf("growth %s: 认证拒绝，中止本轮：%s", uid8, o.report())
		return
	}

	// ── 6. Buddy 能量盲盒 ──────────────────────────────────────────────
	if ctx.Err() != nil {
		return
	}
	if authStop := s.growthBuddyBox(ctx, a, o, isHard); authStop {
		log.Printf("growth %s: 认证拒绝，中止本轮：%s", uid8, o.report())
		return
	}

	// ── 7. 能量 & 连签展示值（纯读，不计失败）──────────────────────────
	if ctx.Err() != nil {
		return
	}
	s.growthTail(ctx, a, o, streakSnap, streakStale)

	log.Printf("growth %s: %s", uid8, o.report())
}

// ── 各段实现 ───────────────────────────────────────────────────────────────

// growthTravel 第 1 段：Buddy 旅行状态机（领礼物 / 派出发）。
// 返回 true 表示遇到认证拒绝，调用方应立即中止本轮。
func (s *Scheduler) growthTravel(ctx context.Context, a *auth.Auth, o *growthOutcome, isHard func(error) bool) bool {
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("查旅行状态失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	state := ts.State
	if state == travelStateArrived {
		// 到站：领礼物。领成功才视为 idle 允许派出，避免覆盖未领取的奖励。
		if ts.RecordID == 0 {
			o.note("旅行已到站但缺 record_id，跳过领奖")
		} else if reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID); err != nil {
			if isAuthErr(err) {
				return true
			}
			o.note("领旅行礼物失败：%v", err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
		} else {
			o.CreditsGained += int(reward)
			o.note("领旅行礼物 +%d 积分", reward)
			o.Successes++
			state = travelStateIdle
		}
	}
	if ctx.Err() != nil {
		return false
	}

	switch {
	case state == travelStateIdle && ts.DailyLimitReached:
		// 服务端明确说今日名额用完：直接收手，不去撞 depart 那堵墙。
		o.note("今日旅行名额已用完")
	case state == travelStateIdle:
		cfg, err := s.cfg.Upstream.GrowthTravelConfigOf(a)
		if err != nil {
			if isAuthErr(err) {
				return true
			}
			o.note("查旅行地点失败：%v", err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if len(cfg.Locations) == 0 {
			o.note("旅行地点列表为空，跳过派出")
			return false
		}
		loc := cfg.Locations[0]
		if err := s.cfg.Upstream.TravelDepart(a, loc.ID); err != nil {
			if isAuthErr(err) {
				return true
			}
			o.note("派 Buddy 失败：%v", err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
			return false
		}
		o.note("派 Buddy 去 %d 号地点（%d 小时后回）", loc.ID, loc.DurationHours)
		o.Successes++
	case state == travelStateTraveling:
		o.note("Buddy 旅行中")
	default:
		o.note("旅行状态未知（%s），跳过", state)
	}
	return false
}

// claimGrowthTasks 第 2 段：任务 accept → 已完成任务 claim。
//
// ⚠️ 2026-10-14 对真实上游实测确认：`GET /activity/growth/tasks` 返回的任务
// **与 accept/claim 不是同一套命名空间**——
//
//	列表返回（新版「养虾」任务）：first_chat / skill_installed / wechat_linked /
//	                              expert_summoned / template_used
//	accept/claim 认的（task_runner.py 那套）：chat_5 / create_canvas / template_5 / ...
//
// 证据：用列表里的 code 去 accept → `task not found`；用 chat_5 去 accept →
// `task already completed` / `already_accepted`，去 claim → 成功 +100 积分 +5 能量。
//
// 因此本段的正确行为是：**只 accept 服务端认的 code**，而列表并不公布它们。
// 现在做法是把 `task not found` 识别为良性命名空间不匹配（不刷失败、不重试），
// 真正的任务点亮与领奖仍由 task_runner.py 承担（taskSchool/taskCat 排程）——
// 那是本仓库已验证可用的实现（能 accept、能 claim 到积分），88lin 不做伪造点亮，
// 它假设列表与 accept 同源，在这个上游版本上不成立。
func (s *Scheduler) claimGrowthTasks(ctx context.Context, a *auth.Auth, o *growthOutcome, isHard func(error) bool) bool {
	tasks, err := s.cfg.Upstream.GrowthTasks(a)
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("查任务列表失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	// 归一化读取：服务端有新旧两代字段名（见 upstream.GrowthTask 注释）。
	titles := map[string]string{}
	pending := []string{}
	for _, t := range tasks {
		code := t.TaskID()
		if code == "" {
			continue
		}
		title := t.Title
		if title == "" {
			title = code
		}
		titles[code] = title
		if !t.Locked && t.State() == "not_accepted" {
			pending = append(pending, code)
		}
	}

	// 分批接单；逐条读 results。`task not found` 是命名空间不匹配（见函数头注释），
	// 按良性处理——否则每个账号每轮都会刷一屏假失败。
	namespaceMismatch := false
	for i := 0; i < len(pending); i += growthTaskAcceptBatch {
		if ctx.Err() != nil {
			return false
		}
		end := i + growthTaskAcceptBatch
		if end > len(pending) {
			end = len(pending)
		}
		results, err := s.cfg.Upstream.GrowthTasksAccept(a, pending[i:end])
		if err != nil {
			if isAuthErr(err) {
				return true
			}
			o.note("接单失败：%v", err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
			continue
		}
		for _, r := range results {
			if r.Status != "error" {
				title := titles[r.TaskCode]
				if title == "" {
					title = r.TaskCode
				}
				o.note("领取任务「%s」（进度开始计）", title)
				o.Successes++
				continue
			}
			if isTaskNotFound(r.Message) {
				namespaceMismatch = true
				continue
			}
			title := titles[r.TaskCode]
			if title == "" {
				title = r.TaskCode
			}
			msg := r.Message
			if msg == "" {
				msg = "服务端未给原因"
			}
			o.note("领取任务「%s」失败：%s", title, msg)
			o.Failures++
		}
	}
	if namespaceMismatch {
		// 一整批都 not found：说明这个上游版本的列表与 accept 不同源，本段无可用任务。
		// 记一条而不是 N 条，且不计失败——它不是故障，是接口形态差异。
		o.note("任务列表与接单接口不同源（列表 code 不被 accept 识别），本段跳过")
	}

	// 领已完成任务的奖励（completed 且未锁）。
	for _, t := range tasks {
		if ctx.Err() != nil {
			return false
		}
		if t.Locked || t.State() != "completed" || t.TaskID() == "" {
			continue
		}
		code := t.TaskID()
		title := titles[code]
		res, err := s.cfg.Upstream.GrowthTaskClaim(a, code)
		if err != nil {
			if isAuthErr(err) {
				return true
			}
			// 「task not completed」与「task not found」同理：列表状态与服务端
			// 状态机不同源，不是失败。
			if isTaskNotCompleted(err) || isTaskNotFound(err.Error()) {
				continue
			}
			o.note("领任务奖「%s」失败：%v", title, err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
			continue
		}
		if res.AlreadyClaimed {
			o.note("任务奖「%s」已领过", title)
			continue
		}
		// 优先用服务端实发值；列表里的 reward_credit 只是活动配置，改版会对不上。
		credit := res.Credit
		if credit == 0 {
			credit = t.RewardCredit
		}
		energy := res.Energy
		if energy == 0 {
			energy = t.RewardEnergy
		}
		o.CreditsGained += credit
		o.note("领任务奖「%s」+%d 积分 +%d 能量", title, credit, energy)
		o.Successes++
	}
	return false
}

// isTaskNotFound 报告错误文案是否为「任务不存在」（命名空间不匹配的良性信号）。
func isTaskNotFound(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "task not found")
}

// isTaskNotCompleted 报告 err 是否为「任务未完成」（HTTP 400 + task not completed）。
func isTaskNotCompleted(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), "task not completed")
}

// growthMakeup 第 3 段：断登自动补一张，保住连登。
//
// 官方规则：补登卡仅可补救当月断登。真正的断点来自 /heatmap 中 score==0 的历史
// 日期；/streak 的 makeup_dates 是**已补登记录**，不能当待办清单。
// 日历结构异常时整段停止（绝不靠猜测消耗补登卡）。
func (s *Scheduler) growthMakeup(ctx context.Context, a *auth.Auth, o *growthOutcome,
	isHard func(error) bool, snap **upstream.GrowthStreakFull, stale *bool) {
	streak, err := s.cfg.Upstream.GrowthStreakSnapshot(a)
	if err != nil {
		o.note("查连登状态失败：%v", err)
		o.Failures++
		o.HardFailures++ // 补登判断所必需的查询，失败必须提醒
		return
	}
	*snap = streak
	if ctx.Err() != nil {
		return
	}
	cards := streak.MakeupCardBalance()
	if cards <= 0 {
		return // 没卡：正常态，不记录
	}

	hm, err := s.cfg.Upstream.GrowthHeatmapOf(a)
	if err != nil {
		o.note("查补登日历失败：%v", err)
		o.Failures++
		o.HardFailures++
		return
	}
	if ctx.Err() != nil {
		return
	}
	dates, err := growthMakeupCandidates(streak, hm)
	if err != nil {
		// 结构异常：停止补登并提醒，绝不猜测日期。
		o.note("补登日历结构异常，已跳过：%v", err)
		o.Failures++
		o.HardFailures++
		return
	}
	if len(dates) == 0 {
		return
	}

	limit := cards
	if limit > makeupMaxPerRun {
		limit = makeupMaxPerRun
	}
	madeUp := 0
	for _, d := range dates[:limit] {
		if ctx.Err() != nil {
			return
		}
		left, err := s.cfg.Upstream.GrowthMakeupUse(a, d)
		if err != nil {
			if upstream.IsMakeupNoNeed(err) {
				// 查询后被客户端或另一轮补上：正常竞争，不当失败。
				o.note("%s 已活跃或已补登，无需再次补登", d)
				break
			}
			if isAuthErr(err) {
				return
			}
			o.note("补登 %s 失败：%v", d, err)
			o.Failures++
			if isHard(err) {
				o.HardFailures++
			}
			break
		}
		madeUp++
		*stale = true // 补登改变连登天数，第 7 段必须重取 streak
		if left < 0 {
			cards--
			left = cards
		} else {
			cards = left
		}
		o.note("补登 %s（剩 %d 张卡）", d, left)
		o.Successes++
	}
	if madeUp > 0 && len(dates) > madeUp && cards > 0 {
		o.note("另有 %d 天可补、剩 %d 张卡，下轮继续", len(dates)-madeUp, cards)
	}
}

// growthRedeem 第 4 段：连登奖励兑换（入门 7d / 进阶 14d / 巅峰 28d）。
// 返回 true 表示认证拒绝。
func (s *Scheduler) growthRedeem(ctx context.Context, a *auth.Auth, o *growthOutcome, isHard func(error) bool) bool {
	summary, err := s.cfg.Upstream.GrowthRedeemSummaryOf(a)
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("查连登兑换状态失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	for _, t := range growthTiers {
		if ctx.Err() != nil {
			return false
		}
		status := growthRedeemStatus(summary, t.Status)
		// 字段缺失或已领/未解锁都跳过——接口改版时不该对三档无脑 POST。
		if status == "" || status == "claimed" || status == "locked" {
			continue
		}
		res, err := s.cfg.Upstream.GrowthRedeem(a, t.Tier, "")
		if err != nil {
			// 档位标识被判 unknown 时退回天数再试一次：这类 400 是参数校验阶段拒绝，
			// 服务端没兑换任何东西，重试不会重复领取。
			if upstream.IsUnknownTier(err) {
				res, err = s.cfg.Upstream.GrowthRedeem(a, strconv.Itoa(t.Days), "")
			}
			if err != nil {
				// 403「连登天数不足」是业务常态，必须先于认证判定。
				if upstream.IsRedeemNotEnoughDays(err) {
					o.note("连登兑换「%s」未解锁（连登天数不足）", t.Label)
					continue
				}
				if upstream.IsRedeemAlreadyClaimed(err) {
					o.note("连登兑换「%s」本月已领过", t.Label)
					continue
				}
				if isAuthErr(err) {
					return true
				}
				o.note("连登兑换「%s」失败：%v", t.Label, err)
				o.Failures++
				if isHard(err) {
					o.HardFailures++
				}
				continue
			}
		}
		o.CreditsGained += res.CreditGranted
		o.note("连登兑换「%s」%s", t.Label, growthRedeemDesc(res, t.Tier))
		o.Successes++
	}
	return false
}

// growthLottery 第 5 段：盲盒抽奖。一轮只开一次（写路径不可逆，剩下的机会留到下轮）。
func (s *Scheduler) growthLottery(ctx context.Context, a *auth.Auth, o *growthOutcome, isHard func(error) bool) bool {
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("查抽奖机会失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if chances <= 0 {
		return false
	}
	res, err := s.cfg.Upstream.GrowthLotteryDraw(a, "")
	if err != nil {
		if upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err) {
			// 次数为 0 / 未开启是常态（次数来自连登兑换），不是故障。
			o.note("开盲盒：无抽奖机会")
			return false
		}
		if isAuthErr(err) {
			return true
		}
		o.note("开盲盒失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	prize := res.PrizeName
	if prize == "" {
		prize = res.PrizeCode
	}
	if prize == "" {
		prize = "未知"
	}
	// 实物奖（冰箱贴/胸针/杯子）要用户自己去成长中心填收件信息，脚本代填不了也不该代填。
	if res.PrizeType == "physical" {
		prize += "（实物奖，需到成长中心填写收件信息）"
	}
	o.note("开盲盒获得：%s", prize)
	o.Successes++
	if chances > 1 {
		o.note("还剩 %d 次抽奖机会，下轮继续", chances-1)
	}
	return false
}

// growthBuddyBox 第 6 段：能量开 Buddy 盲盒（能量没有其它消耗出口）。
func (s *Scheduler) growthBuddyBox(ctx context.Context, a *auth.Auth, o *growthOutcome, isHard func(error) bool) bool {
	quota, err := s.cfg.Upstream.GrowthBuddyQuotaOf(a)
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("查 Buddy 能量失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	if quota.Affordable <= 0 {
		return false
	}
	maxOpen := quota.MaxOpenCount
	if maxOpen <= 0 {
		maxOpen = 1
	}
	count := quota.Affordable
	if count > maxOpen {
		count = maxOpen
	}
	name, err := s.cfg.Upstream.GrowthBuddyOpen(a, count, "")
	if err != nil {
		if isAuthErr(err) {
			return true
		}
		o.note("开 Buddy 盲盒失败：%v", err)
		o.Failures++
		if isHard(err) {
			o.HardFailures++
		}
		return false
	}
	if name == "" {
		name = "新 Buddy"
	}
	o.note("开 Buddy 盲盒 ×%d（%s）", count, name)
	o.Successes++
	return false
}

// growthTail 第 7 段：能量与连签展示值（纯读，不计失败）。
// 复用第 3 段的 streak 快照；补登过（stale）才重取。
func (s *Scheduler) growthTail(ctx context.Context, a *auth.Auth, o *growthOutcome,
	snap *upstream.GrowthStreakFull, stale bool) {
	if energy, err := s.cfg.Upstream.GrowthEnergy(a); err == nil {
		o.Energy = energy
	}
	if ctx.Err() != nil {
		return
	}
	if snap != nil && !stale {
		o.StreakDays = snap.Streak.Days
		return
	}
	if days, err := s.cfg.Upstream.GrowthStreak(a); err == nil {
		o.StreakDays = days
	}
}

// ── 辅助 ───────────────────────────────────────────────────────────────────

// isAuthErr 报告 err 是否为认证/权限拒绝（401/403）。是则调用方应中止本轮。
func isAuthErr(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && (ue.Status == 401 || ue.Status == 403)
}

// growthRedeemStatus 按状态字段前缀取 /redeem/summary 的档位状态。
func growthRedeemStatus(s *upstream.GrowthRedeemSummary, prefix string) string {
	switch prefix {
	case "starter":
		return s.StarterStatus
	case "advanced":
		return s.AdvancedStatus
	case "legendary":
		return s.LegendaryStatus
	}
	return ""
}

// growthRedeemDesc 兑换成功的奖励描述：优先拼服务端实发的 *_granted，
// 挖不到才回落官方文案。注意实发字段名带 _granted 后缀——直接读 credit 恒为空。
func growthRedeemDesc(res *upstream.GrowthRedeemResult, tier string) string {
	bits := []string{}
	if res.CreditGranted > 0 {
		bits = append(bits, fmt.Sprintf("+%d 积分", res.CreditGranted))
	}
	if res.EnergyGranted > 0 {
		bits = append(bits, fmt.Sprintf("+%d 能量", res.EnergyGranted))
	}
	if res.CardsGranted > 0 {
		bits = append(bits, fmt.Sprintf("+%d 补登卡", res.CardsGranted))
	}
	if res.ChancesGranted > 0 {
		bits = append(bits, fmt.Sprintf("+%d 次抽奖", res.ChancesGranted))
	}
	if len(bits) > 0 {
		return "（" + strings.Join(bits, " ") + "）"
	}
	if desc := growthRedeemFallbackDesc[tier]; desc != "" {
		return "（" + desc + "）"
	}
	return "（奖励已到账）"
}

// growthMakeupCandidates 从活跃日历选断登日期。
//
// 规则（88lin _makeup_candidates 同款）：当月、今天之前、活动上线之后、score==0，
// 且不在 makeup_dates（已补登记录）里。用 heatmap.today 的**服务端日期**，避免本机
// 时区把月初/午夜算错。日历缺项不推测为零；结构异常时返回错误让调用方整段停止。
func growthMakeupCandidates(streak *upstream.GrowthStreakFull, hm *upstream.GrowthHeatmap) ([]string, error) {
	parse := func(v string) (time.Time, error) {
		if len(v) != 10 {
			return time.Time{}, fmt.Errorf("日期格式无效：%q", v)
		}
		t, err := time.ParseInLocation("2006-01-02", v, cstZone)
		if err != nil {
			return time.Time{}, fmt.Errorf("日期格式无效：%q", v)
		}
		return t, nil
	}

	todayStr := hm.Today.Date
	if todayStr == "" {
		return nil, fmt.Errorf("活跃日历缺少服务端日期")
	}
	today, err := parse(todayStr)
	if err != nil {
		return nil, err
	}
	// 起始下界：当月 1 号、活动上线日、固定地板日三者取最晚。
	start := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, cstZone)
	launchStr := streak.LaunchDate
	if launchStr == "" {
		launchStr = growthMakeupFloor
	}
	launch, err := parse(launchStr)
	if err != nil {
		return nil, fmt.Errorf("活动上线日无效：%w", err)
	}
	if launch.After(start) {
		start = launch
	}
	if floor, err := parse(growthMakeupFloor); err == nil && floor.After(start) {
		start = floor
	}

	// 已补登记录：不能作为待办清单。
	madeUp := map[string]bool{}
	for _, d := range streak.Streak.MakeupDates {
		if _, err := parse(d); err != nil {
			return nil, fmt.Errorf("已补登记录格式无效：%w", err)
		}
		madeUp[d] = true
	}

	if len(hm.Cells) == 0 {
		return nil, fmt.Errorf("活跃日历缺少日期列表")
	}
	type cand struct {
		day time.Time
		iso string
	}
	var out []cand
	seen := map[string]float64{}
	for _, c := range hm.Cells {
		day, err := parse(c.Date)
		if err != nil {
			return nil, fmt.Errorf("活跃日历记录格式无效：%w", err)
		}
		if prev, dup := seen[c.Date]; dup && prev != c.Score {
			return nil, fmt.Errorf("活跃日历同一日期的记录冲突：%s", c.Date)
		}
		seen[c.Date] = c.Score
		if c.Score < 0 {
			return nil, fmt.Errorf("活跃日历分数无效：%s=%v", c.Date, c.Score)
		}
		if day.Before(start) || !day.Before(today) {
			continue
		}
		if c.Score != 0 || madeUp[c.Date] {
			continue
		}
		out = append(out, cand{day: day, iso: c.Date})
	}
	// 最近的断点优先，以便尽早恢复当前连登。
	sort.Slice(out, func(i, j int) bool { return out[i].day.After(out[j].day) })
	dates := make([]string, 0, len(out))
	for _, c := range out {
		dates = append(dates, c.iso)
	}
	return dates, nil
}
