// ═══ 更新日志 ═══
// 2026-10-14：新增。成长中心完整接口层，语义移植自 88lin/workbuddy-auto-signin
// （MIT，https://github.com/88lin/workbuddy-auto-signin）。
//
// 为什么新增这一层而不是继续用 Python 脚本：签到+成长中心是一条有状态的流水线
// （查状态→签到→查余额→成长中心七段），原来拆在 scripts/growth_center.py 与
// scripts/task_runner.py 两个脚本里，只有 redeem/lottery/makeup 三个子步骤是原生
// 排程，其余（tasks 领奖、补登卡、Buddy 盲盒、energy）根本没有实现。88lin 的脚本
// 把整条链路的接口契约、幂等语义、正常态判定都验证透了，直接移植比在 Go 里重新
// 试错稳。
//
// 路径前缀：88lin 用 /v2/activity/growth/*，本仓库既有代码用 /activity/growth/*
// （不带 /v2）。2026-10-14 对真实 CN 上游逐路径实测：两种前缀**都返回 200**，
// 服务端两者等价。这里沿用本仓库既有写法（不带 /v2），与 travel.go/growth_reward.go
// 保持一致，避免同一个域出现两套路径风格。
package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// 成长中心路径（88lin signin.py 的 base + suffix，实测等价）。
const (
	// checkinStatusPath 查询签到状态（只读，可重试）。
	checkinStatusPath = "/v2/billing/meter/checkin-activity-status"
	// checkinStatusPathNoV2 global 域首选（与 dailyCheckinPath 同构：国际版无 /v2 前缀）。
	checkinStatusPathNoV2 = "/billing/meter/checkin-activity-status"
	// growthTasksPath 任务列表；growthTasksAcceptPath 批量接单（复数数组）。
	growthTasksPath       = "/activity/growth/tasks"
	growthTasksAcceptPath = "/activity/growth/tasks/accept"
	// growthHeatmapPath 活跃日历（补登候选来源）。
	growthHeatmapPath = "/activity/growth/heatmap"
	// growthMakeupUsePath 使用补登卡。
	growthMakeupUsePath = "/activity/growth/makeup-cards/use"
	// growthRedeemSummaryPath 连登兑换各档状态（与 /streak 的 redemption_status 对应）。
	growthRedeemSummaryPath = "/activity/growth/redeem/summary"
	// growthBuddyQuotaPath / growthBuddyOpenPath 能量开 Buddy 盲盒。
	growthBuddyQuotaPath = "/activity/growth/buddy/quota"
	growthBuddyOpenPath  = "/activity/growth/buddy/open"
	// growthEnergyPath 能量余额（纯展示）。
	growthEnergyPath = "/activity/growth/energy"
	// growthTravelConfigPath 旅行地点列表（depart 前取 locations[0].id）。
	growthTravelConfigPath = "/activity/growth/buddy/travel/config"
)

// CheckinStatus 签到状态快照（POST /v2/billing/meter/checkin-activity-status）。
type CheckinStatus struct {
	Active         *bool  `json:"active"`           // 活动是否开启；false = 非签到季
	ActivityName   string `json:"activity_name"`    // 活动名
	TodayCheckedIn bool   `json:"today_checked_in"` // 今日是否已领
	TodayCredit    int    `json:"today_credit"`     // 今日可得积分
	StreakDays     int    `json:"streak_days"`      // 连续天数
	TotalCredits   int    `json:"total_credits"`    // 累计积分
	IsStreakDay    bool   `json:"is_streak_day"`    // 今日是否连签奖励日
}

// CheckinStatusOf 查询签到状态。业务 active=false（非签到季）不是错误，
// 调用方按 Active 字段判断。
//
// 走 billingMeterJSON（billingBase=codebuddy.cn，global 先试无 /v2 再回落）：
// 2026-10-14 实测 copilot.tencent.com 与 www.codebuddy.cn 对这条路径都返回 200，
// 服务端两域等价；用 billingMeterJSON 与既有 DailyCheckin 同一条链路、同一套
// realm 候选路径逻辑，不新开一条风格。
func (c *Client) CheckinStatusOf(a *auth.Auth) (*CheckinStatus, error) {
	data, err := c.billingMeterJSON(a, c.checkinStatusPaths(a), http.MethodPost, map[string]any{})
	if err != nil {
		return nil, err
	}
	var st CheckinStatus
	if len(data) > 0 {
		_ = json.Unmarshal(data, &st)
	}
	return &st, nil
}

// checkinStatusPaths 按 realm 返回签到状态路径候选（与 checkinMeterPaths 同构）。
func (c *Client) checkinStatusPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{checkinStatusPathNoV2, checkinStatusPath}
	}
	return []string{checkinStatusPath}
}

// DailyCheckinCredit 执行签到并返回服务端实发积分（挖不到返回 0，不视为失败）。
//
// 与既有 DailyCheckin 的区别：那个只返回 error，调用方拿不到「领了多少分」。
// 上游成功态返回 {"credit": N}；已签走 IsAlreadyCheckin 判定，与既有语义一致。
func (c *Client) DailyCheckinCredit(a *auth.Auth) (int, error) {
	a = a.Snapshot()
	data, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int `json:"credit"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	return resp.Credit, nil
}

// GrowthTask 成长任务条目（GET /activity/growth/tasks 的 tasks[] 元素）。
//
// ⚠️ 字段契约有**两代形态**，2026-10-14 对真实 CN 上游实测确认：
//
//	旧形态（88lin signin.py 记录，桌面端 growthSpace chunk 读出）：
//	  {"task_code":"...","title":"...","locked":false,
//	   "accept_status":"not_accepted|accepted|in_progress|completed|claimed",
//	   "reward_credit":N,"reward_energy":N}
//	新形态（2026-10-14 实测 5 个账号，返回 5 条任务）：
//	  {"task_id":1,"code":"first_chat","title":"完成一次对话","description":"...",
//	   "level_name":"养虾尝试","status":"available","url":"","icon_url":""}
//
// 服务端已切换到新形态（字段名 code/status，无 accept_status/reward_*）。
// 解析必须两种都认，否则任务段静默空转——这正是「看起来在跑其实什么都没做」
// 的典型形态。Code()/State() 是归一化读取入口，调用方不要直接读字段。
type GrowthTask struct {
	// 旧形态
	TaskCode     string `json:"task_code"`
	Locked       bool   `json:"locked"`
	AcceptStatus string `json:"accept_status"`
	RewardCredit int    `json:"reward_credit"`
	RewardEnergy int    `json:"reward_energy"`
	// 新形态
	Code        string `json:"code"`
	Status      string `json:"status"`
	Description string `json:"description"`
	LevelName   string `json:"level_name"`
	// 两代共有
	Title string `json:"title"`
}

// TaskID 归一化任务标识（新形态 code / 旧形态 task_code）。
func (t GrowthTask) TaskID() string {
	if t.Code != "" {
		return t.Code
	}
	return t.TaskCode
}

// State 归一化任务状态。
//
// 新形态 status 取值：available / completed / claimed（实测 available）；
// 旧形态 accept_status 取值：not_accepted / accepted / in_progress / completed / claimed。
// 两者映射到同一套内部状态：可接单 / 已完成可领 / 已领 / 其它（进行中或锁定）。
func (t GrowthTask) State() string {
	if t.AcceptStatus != "" {
		return t.AcceptStatus
	}
	switch t.Status {
	case "available":
		// 新形态的 available = 可参与（等价旧形态 not_accepted，需要先 accept）。
		return "not_accepted"
	case "completed":
		return "completed"
	case "claimed":
		return "claimed"
	case "locked":
		return "locked"
	}
	// 未知状态按「不动作」处理：宁可漏接一单，也不对未知状态乱发写请求。
	return t.Status
}

// GrowthTasks 拉取任务列表。
func (c *Client) GrowthTasks(a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []GrowthTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// GrowthTaskAcceptResult 单条接单结果。
type GrowthTaskAcceptResult struct {
	TaskCode string `json:"task_code"`
	Status   string `json:"status"` // "error" 表示失败，其余按成功
	Message  string `json:"message"`
}

// GrowthTasksAccept 批量接单（复数数组；旧的单数 task_code 在新服务端一律 400）。
// 返回逐条结果；results 缺失时按 HTTP 状态整体成败填充。
func (c *Client) GrowthTasksAccept(a *auth.Auth, codes []string) ([]GrowthTaskAcceptResult, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	data, err := c.growthJSON(a, http.MethodPost, growthTasksAcceptPath,
		map[string]any{"task_codes": codes})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []GrowthTaskAcceptResult `json:"results"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	if len(resp.Results) == 0 {
		// 服务端没给逐条结果：按整体成功填充，调用方不会漏报成功。
		out := make([]GrowthTaskAcceptResult, 0, len(codes))
		for _, code := range codes {
			out = append(out, GrowthTaskAcceptResult{TaskCode: code, Status: "ok"})
		}
		return out, nil
	}
	return resp.Results, nil
}

// GrowthTaskClaimResult 领任务奖回执。
type GrowthTaskClaimResult struct {
	Credit         int  `json:"credit"`
	Energy         int  `json:"energy"`
	AlreadyClaimed bool `json:"already_claimed"`
}

// GrowthTaskClaim 领取单个任务奖励（路径含 task_code、body 空对象）。
// already_claimed=true 是幂等正常态，调用方不应记为失败。
func (c *Client) GrowthTaskClaim(a *auth.Auth, taskCode string) (*GrowthTaskClaimResult, error) {
	data, err := c.growthJSON(a, http.MethodPost, growthTasksPath+"/"+taskCode+"/claim", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res GrowthTaskClaimResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res)
	}
	return &res, nil
}

// GrowthStreakFull 连登状态完整快照（/streak）。比既有的 GrowthStreak（只取 days）
// 多带 launch_date 与 makeup_dates：补登候选计算需要它们。
type GrowthStreakFull struct {
	LaunchDate string `json:"launch_date"`
	Streak     struct {
		Days        int      `json:"days"`
		MakeupDates []string `json:"makeup_dates"` // 已补登记录（不是待办清单）
	} `json:"streak"`
	MakeupCards json.RawMessage `json:"makeup_cards"` // {"balance":N} 或裸整数 N
}

// GrowthStreakSnapshot 读取连登完整快照。
func (c *Client) GrowthStreakSnapshot(a *auth.Auth) (*GrowthStreakFull, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	var st GrowthStreakFull
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// MakeupCardBalance 解析补登卡余额，兼容 {"balance":N} 与裸整数两种形状。
// 只认前者的话，接口是后者时补登会一声不响地永不执行。
func (s *GrowthStreakFull) MakeupCardBalance() int {
	raw := strings.TrimSpace(string(s.MakeupCards))
	if raw == "" || raw == "null" {
		return 0
	}
	var obj struct {
		Balance int `json:"balance"`
	}
	if strings.HasPrefix(raw, "{") {
		if json.Unmarshal([]byte(raw), &obj) == nil {
			return obj.Balance
		}
		return 0
	}
	var n int
	if json.Unmarshal([]byte(raw), &n) == nil {
		return n
	}
	return 0
}

// makeupCardsBalance 解析任意 makeup_cards 原始 JSON（{"balance":N} 或裸整数）。
func makeupCardsBalance(raw json.RawMessage) int {
	full := GrowthStreakFull{MakeupCards: raw}
	return full.MakeupCardBalance()
}

// GrowthHeatmapCell 活跃日历单元。
type GrowthHeatmapCell struct {
	Date  string  `json:"date"`  // YYYY-MM-DD
	Score float64 `json:"score"` // 0 = 无活跃（补登候选）
}

// GrowthHeatmap 活跃日历（补登候选来源）。Today 是服务端日期，用它而不是本机时区，
// 避免月初/午夜算错。
type GrowthHeatmap struct {
	Today struct {
		Date string `json:"date"`
	} `json:"today"`
	Cells []GrowthHeatmapCell `json:"cells"`
}

// GrowthHeatmapOf 拉取活跃日历。
func (c *Client) GrowthHeatmapOf(a *auth.Auth) (*GrowthHeatmap, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthHeatmapPath, nil)
	if err != nil {
		return nil, err
	}
	var hm GrowthHeatmap
	if err := json.Unmarshal(data, &hm); err != nil {
		return nil, err
	}
	return &hm, nil
}

// GrowthMakeupUse 使用补登卡补 targetDate（YYYY-MM-DD）。返回服务端给出的剩余卡数
// （挖不到返回 -1，调用方自行递减本地计数）。
func (c *Client) GrowthMakeupUse(a *auth.Auth, targetDate string) (int, error) {
	data, err := c.growthJSON(a, http.MethodPost, growthMakeupUsePath,
		map[string]any{"target_date": targetDate})
	if err != nil {
		return -1, err
	}
	var resp struct {
		MakeupCards json.RawMessage `json:"makeup_cards"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &resp)
	}
	if raw := strings.TrimSpace(string(resp.MakeupCards)); raw != "" && raw != "null" {
		return makeupCardsBalance(resp.MakeupCards), nil
	}
	return -1, nil
}

// IsMakeupNoNeed 报告补登被拒是否属于「该日期已活跃或已补登」的良性竞争
// （HTTP 400 + 精确文案 date is not broken, no makeup needed）。
// 查询后被客户端或另一轮补上属正常，不再当失败刷日志。
func IsMakeupNoNeed(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), "date is not broken, no makeup needed")
}

// GrowthRedeemSummary 连登兑换各档状态（GET /redeem/summary）。
// 取值 claimed / locked / 其它（可尝试兑换）。字段名带 _status 后缀。
type GrowthRedeemSummary struct {
	StarterStatus   string `json:"starter_status"`
	AdvancedStatus  string `json:"advanced_status"`
	LegendaryStatus string `json:"legendary_status"`
}

// GrowthRedeemSummaryOf 查询连登兑换状态。
func (c *Client) GrowthRedeemSummaryOf(a *auth.Auth) (*GrowthRedeemSummary, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthRedeemSummaryPath, nil)
	if err != nil {
		return nil, err
	}
	var s GrowthRedeemSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GrowthBuddyQuota 能量开 Buddy 盲盒的额度（GET /buddy/quota）。
type GrowthBuddyQuota struct {
	Affordable   int `json:"affordable"`     // 当前能量够开几个
	MaxOpenCount int `json:"max_open_count"` // 单次上限（缺省按 1）
}

// GrowthBuddyQuotaOf 查询 Buddy 盲盒额度。
func (c *Client) GrowthBuddyQuotaOf(a *auth.Auth) (*GrowthBuddyQuota, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBuddyQuotaPath, nil)
	if err != nil {
		return nil, err
	}
	var q GrowthBuddyQuota
	if len(data) > 0 {
		_ = json.Unmarshal(data, &q)
	}
	return &q, nil
}

// GrowthBuddyOpen 开 Buddy 盲盒，返回获得的 Buddy 名（挖不到返回空串）。
func (c *Client) GrowthBuddyOpen(a *auth.Auth, count int, clientToken string) (string, error) {
	if clientToken == "" {
		clientToken = growthClientToken("buddy")
	}
	data, err := c.growthJSON(a, http.MethodPost, growthBuddyOpenPath,
		map[string]any{"count": count, "client_token": clientToken})
	if err != nil {
		return "", err
	}
	// 名字可能在 buddy / name / buddies 任一处；挖不到不视为失败。
	for _, key := range []string{"buddy", "name", "buddies"} {
		var resp map[string]json.RawMessage
		if json.Unmarshal(data, &resp) != nil {
			break
		}
		raw, ok := resp[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s, nil
		}
	}
	return "", nil
}

// GrowthEnergy 能量余额（GET /energy，纯展示）。
func (c *Client) GrowthEnergy(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthEnergyPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Balance, nil
}

// GrowthTravelConfig 旅行地点列表；取 locations[0].id 供 depart。
type GrowthTravelConfig struct {
	Locations []struct {
		ID            int `json:"id"`
		DurationHours int `json:"duration_hours"`
	} `json:"locations"`
}

// GrowthTravelConfigOf 查询旅行地点配置。
func (c *Client) GrowthTravelConfigOf(a *auth.Auth) (*GrowthTravelConfig, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthTravelConfigPath, nil)
	if err != nil {
		return nil, err
	}
	var cfg GrowthTravelConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
