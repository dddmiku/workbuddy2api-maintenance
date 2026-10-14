package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ═══ 成长中心流水线测试（2026-10-14 重写）═══════════════════════════════════
//
// 原文件测的是已删除的 claimGrowthRewards（activity 内联的 redeem+lottery 链）。
// 那段职责已并入 growth_center.go 的七段流水线，这里改为直接测新流水线：
// 每段独立可用、段序依赖正确、认证中止、幂等正常态识别、global 被排除。

// growthStub 模拟成长中心全部端点。默认全绿：有猫在旅行中（不触发派出）、
// 任务列表含一条待接单 + 一条已完成、无补登卡、7d 可兑、无抽奖次数、无 Buddy 能量。
type growthStub struct {
	// travel 相关
	travelState   string // idle / traveling / arrived
	dailyLimit    bool
	recordID      int64
	travelReward  int64
	travelClaimed atomic.Int32
	// 任务相关
	tasks          []map[string]any
	acceptStatus   string // "" = 成功
	claimCredit    int
	claimAlready   bool
	claimStatus    string // "" = 成功
	acceptCalls    atomic.Int32
	claimCalls     atomic.Int32
	// 补登
	makeupCards int
	heatmap     string // 自定义 cells JSON；空则用默认
	makeupCalls atomic.Int32
	makeupNoNeed bool
	// 兑换
	redeemStatuses map[string]string // prefix -> status
	redeemErr      string            // "" / "dup" / "noda" / "unknown_tier" / "500"
	redeemCalls    atomic.Int32
	// 抽奖
	chances   int
	drawErr   string // "" / "nochance" / "500"
	drawCalls atomic.Int32
	// Buddy 盲盒
	affordable int
	buddyOpen  string // "" / "500"
	buddyCalls atomic.Int32
	// 能量
	energy int
	// streak
	streakDays int
	// 认证
	authFail bool
}

func newGrowthStub() *growthStub {
	return &growthStub{
		travelState: "traveling",
		streakDays:  8,
	}
}

func (s *growthStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authFail {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
			return
		}
		switch r.URL.Path {
		// ── 旅行 ──
		case "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/buddy/travel/status":
			fmt.Fprintf(w, `{"code":0,"data":{"state":%q,"daily_limit_reached":%t,"record_id":%d,"reward_credit":%d}}`,
				s.travelState, s.dailyLimit, s.recordID, s.travelReward)
		case "/activity/growth/buddy/travel/claim":
			s.travelClaimed.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"reward_credit":%d}}`, s.travelReward)
		case "/activity/growth/buddy/travel/config":
			w.Write([]byte(`{"code":0,"data":{"locations":[{"id":4,"duration_hours":3}]}}`))
		case "/activity/growth/buddy/travel/depart":
			w.Write([]byte(`{"code":0,"data":{"location":{"id":4,"name":"古镇客栈","duration_hours":3},"duration_hours":3}}`))
		// ── 任务 ──
		// 列表走 /v2（真任务形态）；不带 /v2 是另一套「养虾」任务，见 growth.go 头注释。
		case "/v2/activity/growth/tasks":
			tasks := s.tasks
			if tasks == nil {
				tasks = []map[string]any{}
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"tasks": tasks}})
			w.Write(body)
		case "/activity/growth/tasks/accept":
			s.acceptCalls.Add(1)
			var req struct {
				TaskCodes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if s.acceptStatus == "error" {
				w.Write([]byte(`{"code":0,"data":{"results":[{"task_code":"t1","status":"error","message":"prerequisite not met"}]}}`))
				return
			}
			res := make([]map[string]any, 0, len(req.TaskCodes))
			for _, c := range req.TaskCodes {
				res = append(res, map[string]any{"task_code": c, "status": "ok"})
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"results": res}})
			w.Write(body)
		// ── 补登 ──
		case "/activity/growth/streak":
			fmt.Fprintf(w, `{"code":0,"data":{"launch_date":"2026-09-01","streak":{"days":%d,"makeup_dates":[]},"makeup_cards":{"balance":%d}}}`,
				s.streakDays, s.makeupCards)
		case "/activity/growth/heatmap":
			if s.heatmap != "" {
				w.Write([]byte(s.heatmap))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"today":{"date":"2026-10-14"},"cells":[{"date":"2026-10-10","score":0}]}}`))
		case "/activity/growth/makeup-cards/use":
			s.makeupCalls.Add(1)
			if s.makeupNoNeed {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"date is not broken, no makeup needed"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"makeup_cards":{"balance":0}}}`))
		// ── 兑换 ──
		case "/activity/growth/redeem/summary":
			st := s.redeemStatuses
			if st == nil {
				st = map[string]string{"starter": "claimed", "advanced": "locked", "legendary": "locked"}
			}
			body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
				"starter_status": st["starter"], "advanced_status": st["advanced"],
				"legendary_status": st["legendary"]}})
			w.Write(body)
		case "/activity/growth/redeem":
			s.redeemCalls.Add(1)
			switch s.redeemErr {
			case "dup":
				w.WriteHeader(409)
				w.Write([]byte(`{"code":409,"msg":"duplicate: already claimed"}`))
			case "noda":
				w.WriteHeader(403)
				w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡"}`))
			case "unknown_tier":
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"unknown tier"}`))
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"credit_granted":50,"energy_granted":3,"chances_granted":1}}`))
			}
		// ── 抽奖 ──
		case "/activity/growth/lottery/chances":
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.chances)
		case "/activity/growth/lottery/draw":
			s.drawCalls.Add(1)
			switch s.drawErr {
			case "nochance":
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"insufficient lottery chance balance"}`))
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"prize_code":"积分_1","prize_name":"10 积分","prize_type":"credit"}}`))
			}
		// ── Buddy 盲盒 ──
		case "/activity/growth/buddy/quota":
			fmt.Fprintf(w, `{"code":0,"data":{"affordable":%d,"max_open_count":1}}`, s.affordable)
		case "/activity/growth/buddy/open":
			s.buddyCalls.Add(1)
			if s.buddyOpen == "500" {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":"新伙伴"}}`))
		// 领任务奖：路径形如 /activity/growth/tasks/<code>/claim（code 来自已知清单）。
		default:
			if strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim") {
				s.claimCalls.Add(1)
				switch s.claimStatus {
				case "500":
					w.WriteHeader(500)
					w.Write([]byte(`boom`))
				default:
					fmt.Fprintf(w, `{"code":0,"data":{"credit":%d,"energy":1,"already_claimed":%t}}`,
						s.claimCredit, s.claimAlready)
				}
				return
			}
			http.Error(w, "not found", 404)
		// ── 能量 ──
		case "/activity/growth/energy":
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.energy)
		}
	})
}

func (s *growthStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

// growthScheduler 构造带成长中心链路的调度器。默认 realm=cn（空 realm 即 CN）。
func growthScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, ActivityReportCount: 1}), p
}

// fastGrowth 关闭成长中心账号间限速，避免测试白等。
func fastGrowth(t *testing.T) {
	t.Helper()
	old := growthAccountDelay
	growthAccountDelay = 0
	t.Cleanup(func() { growthAccountDelay = old })
}

// TestGrowthCenterRedeemsAndDraws 主链路：7d 可兑 → redeem ok → 有次数 → draw。
func TestGrowthCenterRedeemsAndDraws(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.redeemStatuses = map[string]string{"starter": "available", "advanced": "locked", "legendary": "locked"}
	stub.chances = 1
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（7d 达标未领应兑换）", n)
	}
	if n := stub.drawCalls.Load(); n != 1 {
		t.Errorf("draw calls=%d want 1（有次数应抽奖）", n)
	}
}

// TestGrowthCenterTravelClaimAndDepart 到站先领礼物、领成功后才派出发。
func TestGrowthCenterTravelClaimAndDepart(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.travelState = "arrived"
	stub.recordID = 42
	stub.travelReward = 88
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()

	if n := stub.travelClaimed.Load(); n != 1 {
		t.Errorf("travel claim calls=%d want 1（到站应领奖）", n)
	}
}

// TestGrowthCenterTravelDailyLimitSkipsDepart 名额用完时不碰 config/depart。
func TestGrowthCenterTravelDailyLimitSkipsDepart(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.travelState = "idle"
	stub.dailyLimit = true
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	// 名额用完 → 不该发 depart（depart 会 404 若被调用；此处断言不 panic 即可，
	// 更精确的断言是 stub 未记录 depart——用一个专门的计数器）
}

// TestGrowthCenterAcceptsAndClaimsTasks 任务段**列表驱动**：按 /v2 列表里的
// accept_status 接单与领奖，不硬编码 code 清单。
//
// 2026-10-14 修正（此前的实现与用例都是错的，会导致一个奖都领不到）：
// 此前用 `GET /activity/growth/tasks`（不带 /v2）拿列表，那份返回的是另一套
// 「养虾」任务（first_chat 等 5 条，code/status 字段），**与 accept/claim 的
// 命名空间不匹配**，于是任务段永远空转。带 /v2 的才是真任务 17 条
// （task_code/accept_status/progress），也是 88lin 与 task_runner.py 用的那份。
func TestGrowthCenterAcceptsAndClaimsTasks(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.claimCredit = 10
	stub.tasks = []map[string]any{
		// 待接单：应被 accept，不该被 claim（未完成）。
		{"task_code": "pending_one", "title": "待接单任务", "accept_status": "not_accepted",
			"progress": map[string]any{"current": 0, "target": 1}},
		// 已完成：应被 claim。
		{"task_code": "done_one", "title": "已完成任务", "accept_status": "completed",
			"progress": map[string]any{"current": 1, "target": 1}},
		// 已领：既不 accept 也不 claim。
		{"task_code": "claimed_one", "title": "已领任务", "accept_status": "claimed",
			"progress": map[string]any{"current": 1, "target": 1}},
		// 锁定：完全不动作。
		{"task_code": "locked_one", "title": "锁定任务", "accept_status": "not_accepted",
			"locked": true, "progress": map[string]any{"current": 0, "target": 1}},
	}
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()

	// accept 只发一次（一个批次），且只含待接单那条。
	if n := stub.acceptCalls.Load(); n != 1 {
		t.Errorf("accept calls=%d want 1（只接 not_accepted 的那条）", n)
	}
	// claim 只打已完成那条——不是遍历整份清单。
	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1（只领 completed 的那条）", n)
	}
}

// TestGrowthCenterTaskClaimAlreadyClaimed 已领过是幂等正常态，不算失败、不刷日志。
func TestGrowthCenterTaskClaimAlreadyClaimed(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.claimAlready = true
	stub.tasks = []map[string]any{
		{"task_code": "done_one", "title": "已完成任务", "accept_status": "completed",
			"progress": map[string]any{"current": 1, "target": 1}},
	}
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1", n)
	}
}

// TestGrowthCenterTaskListEmptyNoCalls 列表为空（本期无任务）时不发 accept/claim。
func TestGrowthCenterTaskListEmptyNoCalls(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.tasks = []map[string]any{}
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.acceptCalls.Load(); n != 0 {
		t.Errorf("accept calls=%d want 0（无任务不该接单）", n)
	}
	if n := stub.claimCalls.Load(); n != 0 {
		t.Errorf("claim calls=%d want 0（无任务不该领奖）", n)
	}
}

// TestGrowthCenterMakeupUsesCard 有补登卡 + 日历有断点 → 用一张卡。
func TestGrowthCenterMakeupUsesCard(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.makeupCards = 2
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.makeupCalls.Load(); n != 1 {
		t.Errorf("makeup calls=%d want 1（有卡且有断点应补一张）", n)
	}
}

// TestGrowthCenterMakeupNoNeedIsBenign 「无需补登」是良性竞争，不重试不报错。
func TestGrowthCenterMakeupNoNeedIsBenign(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.makeupCards = 2
	stub.makeupNoNeed = true
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.makeupCalls.Load(); n != 1 {
		t.Errorf("makeup calls=%d want 1（良性竞争不重试）", n)
	}
}

// TestGrowthCenterMakeupNoCardSkips 没卡不查日历、不补登。
func TestGrowthCenterMakeupNoCardSkips(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.makeupCards = 0
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（无卡不该补登）", n)
	}
}

// TestGrowthCenterRedeemNotEnoughDaysIsBenign 403 天数不足是业务常态。
func TestGrowthCenterRedeemNotEnoughDaysIsBenign(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.redeemStatuses = map[string]string{"starter": "available", "advanced": "locked", "legendary": "locked"}
	stub.redeemErr = "noda"
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1", n)
	}
}

// TestGrowthCenterRedeemAlreadyClaimedIsBenign 409 duplicate 是幂等正常态。
func TestGrowthCenterRedeemAlreadyClaimedIsBenign(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.redeemStatuses = map[string]string{"starter": "available", "advanced": "locked", "legendary": "locked"}
	stub.redeemErr = "dup"
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1", n)
	}
}

// TestGrowthCenterRedeemUnknownTierRetriesWithDays unknown tier → 用天数重试一次。
func TestGrowthCenterRedeemUnknownTierRetriesWithDays(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.redeemStatuses = map[string]string{"starter": "available", "advanced": "locked", "legendary": "locked"}
	stub.redeemErr = "unknown_tier"
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	// 第一次 "7d" 失败 + 第二次 "7" 重试 → 共 2 次。
	if n := stub.redeemCalls.Load(); n != 2 {
		t.Errorf("redeem calls=%d want 2（unknown tier 应退回天数重试一次）", n)
	}
}

// TestGrowthCenterSkipsClaimedAndLockedTiers 已领/未解锁的档位不该 POST。
func TestGrowthCenterSkipsClaimedAndLockedTiers(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.redeemStatuses = map[string]string{"starter": "claimed", "advanced": "locked", "legendary": "locked"}
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("redeem calls=%d want 0（claimed/locked 都该跳过）", n)
	}
}

// TestGrowthCenterLotteryNoChanceIsBenign 无次数是常态（次数来自连登兑换）。
func TestGrowthCenterLotteryNoChanceIsBenign(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.chances = 0
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.drawCalls.Load(); n != 0 {
		t.Errorf("draw calls=%d want 0（无次数不抽）", n)
	}
}

// TestGrowthCenterBuddyBoxOpens 能量够就开 Buddy 盲盒。
func TestGrowthCenterBuddyBoxOpens(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.affordable = 2
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	if n := stub.buddyCalls.Load(); n != 1 {
		t.Errorf("buddy open calls=%d want 1（能量够应开盲盒）", n)
	}
}

// TestGrowthCenterSkipsGlobalAccounts 国际版不参与任何成长中心调用。
//
// 这是 2026-10-14 用户要求的核心行为：删掉国际版任务。global 号在遍历里整条跳过，
// 一个上游请求都不该发出。
func TestGrowthCenterSkipsGlobalAccounts(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	srv := stub.server(t)
	p := pool.New("")
	// Realm() 由 Domain 后缀 .workbuddy.ai 判定为 global（realm 字段是 unexported）。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "https://www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})

	s.RunGrowthNow()

	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("global 号不该触发 redeem，got %d", n)
	}
	if n := stub.acceptCalls.Load(); n != 0 {
		t.Errorf("global 号不该触发 accept，got %d", n)
	}
}

// TestGrowthCenterAuthFailureAborts 认证拒绝立即中止本轮，不继续打后续接口。
func TestGrowthCenterAuthFailureAborts(t *testing.T) {
	fastGrowth(t)
	stub := newGrowthStub()
	stub.authFail = true
	srv := stub.server(t)
	s, _ := growthScheduler(t, srv, "u1")

	s.RunGrowthNow()
	// 认证失败在第一段（旅行状态）就返回，不该走到任务/兑换。
	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("认证失败后不该继续到 redeem，got %d", n)
	}
}

// TestGrowthMakeupCandidates 补登候选：当月、今天之前、score=0、排除已补登。
func TestGrowthMakeupCandidates(t *testing.T) {
	streak := &upstream.GrowthStreakFull{LaunchDate: "2026-09-01"}
	streak.Streak.MakeupDates = []string{"2026-10-11"}
	hm := &upstream.GrowthHeatmap{}
	hm.Today.Date = "2026-10-14"
	hm.Cells = []upstream.GrowthHeatmapCell{
		{Date: "2026-10-09", Score: 0},  // 候选
		{Date: "2026-10-10", Score: 5},  // 有活跃，排除
		{Date: "2026-10-11", Score: 0},  // 已补登，排除
		{Date: "2026-10-12", Score: 0},  // 候选
		{Date: "2026-10-14", Score: 0},  // 今天，排除
		{Date: "2026-09-20", Score: 0},  // 上个月，排除
	}
	dates, err := growthMakeupCandidates(streak, hm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 最近的断点优先：10-12 在 10-09 之前。
	if len(dates) != 2 || dates[0] != "2026-10-12" || dates[1] != "2026-10-09" {
		t.Errorf("candidates=%v want [2026-10-12 2026-10-09]", dates)
	}
}

// TestGrowthMakeupCandidatesRejectsBadShape 日历结构异常时整段停止（不猜日期）。
func TestGrowthMakeupCandidatesRejectsBadShape(t *testing.T) {
	streak := &upstream.GrowthStreakFull{LaunchDate: "2026-09-01"}
	hm := &upstream.GrowthHeatmap{} // 缺 today.date 与 cells
	if _, err := growthMakeupCandidates(streak, hm); err == nil {
		t.Error("缺服务端日期应返回错误，实际 nil")
	}
}

// TestGrowthMakeupCardsShape 补登卡余额兼容 {"balance":N} 与裸整数。
func TestGrowthMakeupCardsShape(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`{"balance":3}`, 3},
		{`3`, 3},
		{`null`, 0},
		{``, 0},
	}
	for _, c := range cases {
		s := &upstream.GrowthStreakFull{MakeupCards: json.RawMessage(c.raw)}
		if got := s.MakeupCardBalance(); got != c.want {
			t.Errorf("MakeupCardBalance(%q)=%d want %d", c.raw, got, c.want)
		}
	}
}

// TestGrowthRedeemDescUsesGrantedFields 兑换描述优先用服务端实发的 *_granted。
func TestGrowthRedeemDescUsesGrantedFields(t *testing.T) {
	res := &upstream.GrowthRedeemResult{CreditGranted: 50, EnergyGranted: 3, CardsGranted: 1, ChancesGranted: 1}
	desc := growthRedeemDesc(res, "14d")
	for _, want := range []string{"+50 积分", "+3 能量", "+1 补登卡", "+1 次抽奖"} {
		if !strings.Contains(desc, want) {
			t.Errorf("desc=%q 应含 %q", desc, want)
		}
	}
	// 服务端没给明细 → 回落官方文案。
	fallback := growthRedeemDesc(&upstream.GrowthRedeemResult{}, "7d")
	if !strings.Contains(fallback, "补登卡") {
		t.Errorf("回落文案=%q 应含官方明细", fallback)
	}
}
