// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import "fmt"

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 多类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 /
// 连登兑换 / 成长抽奖 / 补签。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	CheckinHours []int `json:"checkin_hours"` // [9,21]（WorkBuddy 两域）
	// TraeCheckHours 是 Trae 站点的独立签到排程。Trae 与 WorkBuddy 不是同一个
	// 站点（api.trae.cn vs codebuddy.cn），签到必须分开排程、分开开关。
	TraeCheckHours []int `json:"trae_check_hours"` // [11]
	TravelHours    []int `json:"travel_hours"`     // [15]
	ActivityHours  []int `json:"activity_hours"`   // [10]
	KeepaliveHours []int `json:"keepalive_hours"`  // [22]
	SchoolHours    []int `json:"school_hours"`     // [13] 开学季任务（迁移自 school/cat 两条系统 crontab）
	CatHours       []int `json:"cat_hours"`        // [1] 夜猫窗口 23-08 CST，01:00 窗口内补 1 次
	// GrowthHours 是**成长中心**的排程（2026-10-14 新增，合并原 redeem/lottery/makeup）。
	//
	// 为什么合并成一组：成长中心是一条有依赖的流水线——领任务奖送的抽奖次数当场就能抽，
	// 连登兑换送的补登卡当场就能补，补登又会改变连登天数进而影响兑换资格。原来拆成
	// redeem(9 点)/lottery(21 点)/makeup(9 点) 三条独立排程，补登与兑换撞在同一小时靠
	// 先后顺序碰运气，抽奖要等到 21 点而任务奖可能 9 点就发了（次数空放半天）。
	// 合并后一次跑完全部七段，顺序由代码保证（88lin 的 run_growth 同款）。
	//
	// 只服务 CN：国际版（global）不参与任何成长中心调用。
	GrowthHours []int `json:"growth_hours"` // [12]
	// BalanceHours 是**余额刷新**的独立排程（2026-10-14 新增）。
	//
	// 为什么必须独立：积分余额与快过期分桶原本只在签到任务里刷新（checkinAll 调
	// SetCreditsDetailed），而多客户端共用同一批账号的用户**必须关掉签到**（否则
	// 两个实例同时签到会触发上游风控）。关掉签到的副作用是池里 credits 恒为 0、
	// credits_expiring 恒为 0，于是「优先消耗快过期积分」这条选号规则静默失效——
	// 用户看到面板有数据（面板直查上游），却不知道网关选号时两眼一抹黑。
	//
	// 余额查询是**纯读**接口（不写上游状态、不消耗账号），单独跑不引入风控风险，
	// 所以把它从签到里解耦出来，各自可开关。
	BalanceHours []int `json:"balance_hours"` // [14]
	// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/SchoolEnabled/CatEnabled
	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled   bool `json:"checkin_enabled"`    // 缺省 true；false = 关 WorkBuddy 签到
	TraeCheckEnabled bool `json:"trae_check_enabled"` // 缺省 true；false = 关 Trae 签到
	TravelEnabled    bool `json:"travel_enabled"`     // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`   // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"`  // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`     // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`        // 缺省 true；false = 停夜猫子任务
	// GrowthEnabled 成长中心开关（缺省 true）。false = 完全停成长中心流水线。
	GrowthEnabled bool `json:"growth_enabled"`
	// BalanceEnabled 余额刷新开关（缺省 true）。
	//
	// 默认开：它只读上游、不改任何状态，是「快过期积分优先消耗」的数据来源。
	// 关掉它的唯一合理场景是彻底不想让网关周期性访问上游。
	BalanceEnabled bool `json:"balance_enabled"` // 缺省 true；false = 停余额刷新
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`
	// 猫猫旅行已退役 travel_interval_minutes：旅行现为独立排程（travel_hours）。
	// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
//
// **小时必须错开**（2026-10-22 用户反馈「定时重复的任务挤在一起」）。
// 旧默认 checkin/travel/growth/balance 全排在 [9,21]，四个任务同一分钟启动：
// 签到要打签到接口、成长中心要打任务接口、余额刷新要打资源接口，同一账号在
// 同一秒被三四个请求打中，既触发上游风控，日志也搅在一起看不出谁失败。
//
// 现在的分配原则：每个整点最多一个「重」任务，间隔 ≥1 小时。
//   09:00  签到（WorkBuddy）       —— 早上第一件事，决定当天能否用
//   10:00  活跃上报                —— 签到后 1 小时，避免与签到抢同一账号
//   11:00  Trae 签到               —— Trae 是独立站点，与 CN 无账号冲突
//   12:00  成长中心                —— 任务类，依赖签到后的余额
//   13:00  开学季任务
//   14:00  余额刷新                —— 纯读，放在重任务之后
//   15:00  猫猫旅行
//   21:00  签到（WorkBuddy）       —— 晚间补签
//   22:00  token 保活              —— 睡前刷新，覆盖次日
//   01:00  夜猫子任务              —— 夜猫窗口 23-08 内
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:        []int{9, 21},
		TraeCheckHours:      []int{11},
		TravelHours:         []int{15},
		ActivityHours:       []int{10},
		KeepaliveHours:      []int{22},
		SchoolHours:         []int{13},
		CatHours:            []int{1},
		GrowthHours:         []int{12},
		BalanceHours:        []int{14},
		CheckinEnabled:      true,
		TraeCheckEnabled:    true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		GrowthEnabled:       true,
		BalanceEnabled:      true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，ActivityReportCount 归一，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），在此补齐。
// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
//
// ActivityReportCount：0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
// 注意这是「显式配 0 = 旧行为」的兼容语义，与 scheduler.New 的 <=0 → 1 归一一致；
// 「缺省 = 5」由 DefaultSchedule 在 Unmarshal 前置入，是另一条路径，两者不合并。
func (s *Schedule) Normalize() error {
	// 回落值统一取 DefaultSchedule，**不再在这里抄一份字面量**。
	// 2026-10-22 踩到：DefaultSchedule 里把 travel 从 [9,21] 改成 [15] 后，
	// 这里还留着旧值 [9,21]，于是「显式配了空数组」的配置会悄悄回落到旧排程，
	// 而测试同时断言两处、立刻红——两处漂移是这类改动的固定失败模式。
	def := DefaultSchedule()
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = def.CheckinHours
	}
	if len(s.TraeCheckHours) == 0 {
		s.TraeCheckHours = def.TraeCheckHours
	}
	if len(s.TravelHours) == 0 {
		s.TravelHours = def.TravelHours
	}
	if len(s.ActivityHours) == 0 {
		s.ActivityHours = def.ActivityHours
	}
	if len(s.KeepaliveHours) == 0 {
		s.KeepaliveHours = def.KeepaliveHours
	}
	if len(s.SchoolHours) == 0 {
		s.SchoolHours = def.SchoolHours
	}
	if len(s.CatHours) == 0 {
		s.CatHours = def.CatHours
	}
	if len(s.GrowthHours) == 0 {
		s.GrowthHours = def.GrowthHours
	}
	if len(s.BalanceHours) == 0 {
		s.BalanceHours = def.BalanceHours
	}
	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	return s.validateHours()
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.trae_check_hours", "trae_check_enabled", s.TraeCheckHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", s.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", s.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", s.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.school_hours", "school_enabled", s.SchoolHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.cat_hours", "cat_enabled", s.CatHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.growth_hours", "growth_enabled", s.GrowthHours); err != nil {
		return err
	}
	return checkHourRange("schedule.balance_hours", "balance_enabled", s.BalanceHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
