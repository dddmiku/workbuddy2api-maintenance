// ═══ 更新日志 ═══
// 2026-10-15：初版——Trae 签到状态机与 9074 重试循环的回归。
//
// 本文件锁定的核心结论：**9074 不是「当天放弃」的理由**。
// 线上实测（2026-10-14/15 连续两天）16420377 与 41168546 都是 ok=4 fail=2，
// 同一对号固定失败——根因是签到排程一天只跑一次（trae_check_hours=[10]），
// 10 点撞上 9074 窗口后当天再无机会，而退避表 traeBackoff 只写不读、
// 根本没有消费者。下面用回归钉住「退避到期后会被自动重试」。
package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

// ── 状态机纯函数 ────────────────────────────────────────────────────────

// TestTraeCheckedInTodayUsesCSTBusinessDay 「今天已签」按 CST 业务日判定，跨天自动失效。
//
// 必须用 UTC+8 而不是本机时区：网关跑在容器里通常是 UTC，用本地时区会让「今天」
// 在早上 8 点前错位到昨天，刚签过的账号会被反复重签。
func TestTraeCheckedInTodayUsesCSTBusinessDay(t *testing.T) {
	// 北京时间 2026-10-15 00:30 == UTC 2026-10-14 16:30。
	signedAt := time.Date(2026, 10, 15, 0, 30, 0, 0, traeCheckinLocation)
	st := traeCheckinState{CheckedInAt: signedAt.Unix()}

	sameDay := time.Date(2026, 10, 15, 23, 59, 0, 0, traeCheckinLocation)
	if !traeCheckedInToday(st, sameDay) {
		t.Error("同一天（CST）应判定为今天已签")
	}
	// 跨天即失效，不需要额外的清理任务。
	nextDay := time.Date(2026, 10, 16, 0, 1, 0, 0, traeCheckinLocation)
	if traeCheckedInToday(st, nextDay) {
		t.Error("跨天后必须失效——否则第二天永远不会再签")
	}
	// 零值（从未签过）不算已签。
	if traeCheckedInToday(traeCheckinState{}, sameDay) {
		t.Error("零值状态不该判定为已签")
	}
	// 关键边界：同一时刻用 UTC 表示，判定结果必须一致（证明用的是 CST 不是本机时区）。
	if !traeCheckedInToday(st, sameDay.UTC()) {
		t.Error("用 UTC 表示的同一时刻应得到相同判定（判定必须固定用 CST）")
	}
}

// TestTraeBackoffRemainingSurvivesRestart 退避用墙钟起算，跨重启仍能算出剩余时间。
//
// 用单调时钟（内存 until）重启即归零，算出来的等待是负的——容器重启后会立刻再打，
// 等于把刚被限流的窗口又延长一次。
func TestTraeBackoffRemainingSurvivesRestart(t *testing.T) {
	now := time.Now()
	st := traeCheckinState{RetryBackoff: 120, RetryUpdatedAt: now.Unix()}
	// 模拟「重启后从文件读回」：同样的字段，重新算剩余。
	if d := traeBackoffRemaining(st, now.Add(30*time.Second)); d <= 0 || d > 120*time.Second {
		t.Fatalf("重启后应算出剩余退避，实际 %s", d)
	}
	if d := traeBackoffRemaining(st, now.Add(121*time.Second)); d > 0 {
		t.Fatalf("退避已过应返回 0，实际 %s", d)
	}
	// 未记录退避 → 不等待。
	if d := traeBackoffRemaining(traeCheckinState{}, now); d > 0 {
		t.Fatalf("无退避记录应返回 0，实际 %s", d)
	}
}

// TestTraeRetryPendingCriteria 重试循环的筛选判据。
//
// DeviceGeneration 参与判据是关键：退避清零之后、签上之前，账号仍应被循环看到
// （参考实现同一口径，并特意说明「this loop must not turn into a general claim
// poller」——没有 9074 痕迹的账号归每日排程管，不该被这个循环持续打）。
func TestTraeRetryPendingCriteria(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		st   traeCheckinState
		want bool
	}{
		{"全新账号（无 9074 痕迹）", traeCheckinState{}, false},
		{"有退避", traeCheckinState{RetryBackoff: 60, RetryUpdatedAt: now.Unix()}, true},
		{"退避清零但计数还在", traeCheckinState{Retry9074Count: 2}, true},
		{"退避与计数都清了但代数还在", traeCheckinState{DeviceGeneration: 1}, true},
		{
			"今天已签（即使有 9074 痕迹也不再重试）",
			traeCheckinState{Retry9074Count: 3, DeviceGeneration: 2, CheckedInAt: now.Unix()},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := traeRetryPending(tc.st, now); got != tc.want {
				t.Errorf("traeRetryPending=%v，应为 %v", got, tc.want)
			}
		})
	}
}

// TestTraeClearBackoffKeepsDeviceGeneration 清退避时必须保留设备代数。
//
// 代数记录的是「哪个设备号还能用」，与今天签没签无关。清了它下次又要从被限流的
// gen0 开始白撞一遍（实测 2026-10-12 00:00 那两个号连 gen0→gen1 都吃 9074）。
// 参考实现 _checkin_clear_retry_state 同样只清 retry_backoff/retry_9074_count。
func TestTraeClearBackoffKeepsDeviceGeneration(t *testing.T) {
	s := New(Config{})
	s.noteTraeDeviceGeneration("u1", 2)
	s.noteTraeBackoff("u1")
	s.clearTraeBackoff("u1")

	st := s.traeState("u1")
	if st.RetryBackoff != 0 || st.Retry9074Count != 0 {
		t.Errorf("退避应清零，实际 %+v", st)
	}
	if st.DeviceGeneration != 2 {
		t.Fatalf("设备代数应保留为 2，实际 %d（清掉会重走被限流的 gen0）", st.DeviceGeneration)
	}
}

// TestTraeDeviceGenerationOnlyAdvances 代数只增不减（并发重试不会把代数改小）。
func TestTraeDeviceGenerationOnlyAdvances(t *testing.T) {
	s := New(Config{})
	s.noteTraeDeviceGeneration("u1", 3)
	s.noteTraeDeviceGeneration("u1", 1) // 迟到的旧值
	if got := s.traeDeviceGeneration("u1"); got != 3 {
		t.Fatalf("代数应保持 3（旧值不得回退），实际 %d", got)
	}
}

// ── 落盘 ────────────────────────────────────────────────────────────────

// TestTraeCheckinStatePersistsAcrossRestart 设备代数/退避/已签缓存都要跨重启保留。
//
// 签到排程默认一天只跑一次，重启恰好落在签到点前后就会让当天的重试进度作废——
// 这正是把状态从内存搬到 trae-checkin.json 的原因。
func TestTraeCheckinStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first := New(Config{StateDir: dir})
	first.noteTraeDeviceGeneration("u1", 2)
	first.noteTraeBackoff("u1")
	first.markTraeCheckedIn("u2", 100)

	// 模拟重启：用同一个目录重新构造。
	second := New(Config{StateDir: dir})
	if got := second.traeDeviceGeneration("u1"); got != 2 {
		t.Errorf("重启后设备代数应仍为 2，实际 %d", got)
	}
	if st := second.traeState("u1"); st.Retry9074Count == 0 || st.RetryBackoff == 0 {
		t.Errorf("重启后退避记录应仍在，实际 %+v", st)
	}
	if !traeCheckedInToday(second.traeState("u2"), time.Now()) {
		t.Error("重启后「今天已签」的缓存应仍有效")
	}

	// 文件确实落在 StateDir 下且是合法 JSON。
	raw, err := os.ReadFile(filepath.Join(dir, traeCheckinFileName))
	if err != nil {
		t.Fatalf("状态文件应存在：%v", err)
	}
	var payload struct {
		Accounts map[string]traeCheckinState `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("状态文件应是合法 JSON：%v", err)
	}
	if payload.Accounts["u1"].DeviceGeneration != 2 {
		t.Errorf("文件里的代数应为 2，实际 %+v", payload.Accounts["u1"])
	}
}

// TestTraeCheckinStateToleratesCorruptFile 状态文件损坏不该让调度器构造失败。
//
// 签到状态丢了只是重试进度归零，不影响正确性（9074 是时段性的，重试本身安全），
// 所以容错即可，不像 pool 的 state.json 那样必须隔离。
func TestTraeCheckinStateToleratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, traeCheckinFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{StateDir: dir})
	if got := s.traeDeviceGeneration("u1"); got != 0 {
		t.Errorf("损坏文件应退化为空状态，实际代数 %d", got)
	}
	// 仍要能正常写入（覆盖损坏文件）。
	s.noteTraeDeviceGeneration("u1", 1)
	if got := New(Config{StateDir: dir}).traeDeviceGeneration("u1"); got != 1 {
		t.Errorf("损坏文件被覆盖后应能读回，实际 %d", got)
	}
}

// ── 重试循环 ────────────────────────────────────────────────────────────

// traeCheckinStub 假 trae 上游：status 与 claim 的响应可编程。
type traeCheckinStub struct {
	claimCalls atomic.Int64
	statusBody string
	// claimBodies 按调用序号返回；用尽后返回最后一条。
	claimBodies []string
}

func (s *traeCheckinStub) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 按路径分流：status 与 claim 是两个端点，混在一起会让「查状态」拿到
		// claim 的响应，测出来的行为与生产完全不是一回事。
		if strings.HasSuffix(r.URL.Path, "/claim") {
			idx := int(s.claimCalls.Add(1)) - 1
			if idx >= len(s.claimBodies) {
				idx = len(s.claimBodies) - 1
			}
			if idx >= 0 {
				_, _ = w.Write([]byte(s.claimBodies[idx]))
				return
			}
		}
		_, _ = w.Write([]byte(s.statusBody))
	}))
	old := trae.TraeAPIHost
	trae.TraeAPIHost = srv.URL
	t.Cleanup(func() {
		trae.TraeAPIHost = old
		srv.Close()
	})
}

// newTraeS 构造只含一个 trae 账号的调度器（上游全被 stub 接管）。
//
// 账号走 auth.Parse 的嵌套形（auth.realm = "trae"）而不是手搓 Auth 结构：
// realm 是未导出字段，Realm() 靠它判定，只有走真实解析路径才能保证
// traeCheckAll 的「只处理 trae 域」分流真的命中。
func newTraeS(t *testing.T, stub *traeCheckinStub, dir string) (*Scheduler, *pool.Pool) {
	t.Helper()
	stub.start(t)
	raw := []byte(`{
	  "account": {"uid": "2081842786229369", "nickname": "t"},
	  "auth": {
	    "accessToken": "tok", "refreshToken": "refresh",
	    "expiresAt": 4102444800, "realm": "trae"
	  }
	}`)
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("构造 trae 账号失败：%v", err)
	}
	a.FilePath = filepath.Join(t.TempDir(), "a.json")

	p := pool.New("")
	p.Add(a)
	s := New(Config{
		Pool: p, Upstream: upstream.New(), StateDir: dir,
		// 把全局 claim 节流压到 1ms：默认 60s 会让每个用例真等一分钟。
		// 节流逻辑本身由 TestTraeClaimGapThrottlesGlobally 单独覆盖。
		TraeClaimGap:      time.Millisecond,
		TraeRetryInterval: 50 * time.Millisecond,
	})
	return s, p
}

// TestTraeRetryCycleRetriesAfter9074 9074 退避到期后，重试循环必须真的再签一次。
//
// 这是本次重构的核心：原先 traeBackoff 只写不读、没有任何消费者，10 点撞上
// 9074 就整天放弃（线上 2026-10-14/15 两天固定 ok=4 fail=2）。
func TestTraeRetryCycleRetriesAfter9074(t *testing.T) {
	// status 说没签；claim 前两次都 9074（gen0 与换后的 gen1 都被限流），
	// 第三次（重试循环里）成功——模拟「窗口开了」。
	stub := &traeCheckinStub{
		statusBody: `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{
			`{"code":9074,"message":"限流"}`,
			`{"code":9074,"message":"限流"}`,
			`{"code":0,"message":"success","credits":100}`,
		},
	}
	s, _ := newTraeS(t, stub, t.TempDir())

	// 第一轮：撞 9074，留下退避 + 代数。
	if _, err := s.traeCheckAll(context.Background()); err != nil {
		t.Fatalf("首轮签到不该报错：%v", err)
	}
	state := s.traeState("2081842786229369")
	if state.Retry9074Count == 0 {
		t.Fatal("9074 后应记下重试计数")
	}
	if state.DeviceGeneration == 0 {
		t.Fatal("9074 后应记下换过的设备代数")
	}
	firstCalls := stub.claimCalls.Load()

	// 退避未到期：重试循环应跳过（不撞上游）。
	s.traeRetryCycle(context.Background())
	if got := stub.claimCalls.Load(); got != firstCalls {
		t.Fatalf("退避未到期不该再打上游，claim 次数 %d → %d", firstCalls, got)
	}

	// 把退避推到过去（模拟「等到了」），重试循环应再签一次并成功。
	s.traeCheckin.update("2081842786229369", func(st *traeCheckinState) {
		st.RetryUpdatedAt = time.Now().Add(-time.Hour).Unix()
	})
	s.traeRetryCycle(context.Background())
	if got := stub.claimCalls.Load(); got <= firstCalls {
		t.Fatalf("退避到期后应重试，claim 次数仍为 %d", got)
	}
	after := s.traeState("2081842786229369")
	if !traeCheckedInToday(after, time.Now()) {
		t.Fatal("重试成功后应记为「今天已签」")
	}
	if after.Retry9074Count != 0 {
		t.Errorf("成功后应清 9074 计数，实际 %d", after.Retry9074Count)
	}
	if after.DeviceGeneration == 0 {
		t.Error("成功后应保留设备代数（清掉下次要重走被限流的 gen0）")
	}
}

// TestTraeRetryCycleSkipsAlreadyCheckedIn 今天已签的账号不该被重试循环再打一次。
func TestTraeRetryCycleSkipsAlreadyCheckedIn(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":9074,"message":"限流"}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	// 手动标成「今天已签」，同时留一个 9074 痕迹。
	s.markTraeCheckedIn("2081842786229369", 100)
	s.noteTraeBackoff("2081842786229369")
	s.traeCheckin.update("2081842786229369", func(st *traeCheckinState) {
		st.RetryUpdatedAt = time.Now().Add(-time.Hour).Unix()
	})
	before := stub.claimCalls.Load()
	s.traeRetryCycle(context.Background())
	if got := stub.claimCalls.Load(); got != before {
		t.Fatalf("今天已签的账号不该被重试循环打上游，claim 次数 %d → %d", before, got)
	}
}

// TestTraeRetryCycleIgnoresAccountsWithout9074 没有 9074 痕迹的账号不该被循环碰。
//
// 参考实现特意说明「this loop must not turn into a general claim poller」——
// 否则它会对全部 trae 账号形成持续压力，把「重试」变成「轮询」。
func TestTraeRetryCycleIgnoresAccountsWithout9074(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success"}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	// 全新账号：没有任何 9074 痕迹。
	s.traeRetryCycle(context.Background())
	if got := stub.claimCalls.Load(); got != 0 {
		t.Fatalf("无 9074 痕迹的账号不该被重试循环碰，实际 claim %d 次", got)
	}
}

// TestTraeCheckAllSkipsCheckedInToday 定时入口对「今天已签」的账号不再打上游。
//
// 这一层缓存原先没有：每天定时点之后的每一次手动触发（以及新加的自动重试循环）
// 都会重打一遍 status + claim。
func TestTraeCheckAllSkipsCheckedInToday(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success"}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	s.markTraeCheckedIn("2081842786229369", 100)

	out, err := s.traeCheckAll(context.Background())
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := stub.claimCalls.Load(); got != 0 {
		t.Fatalf("今天已签不该再 claim，实际 %d 次", got)
	}
	if len(out) != 1 || out[0].Status != CheckinAlready {
		t.Fatalf("应报告为已签，实际 %+v", out)
	}
}

// TestTraeCheckAllSkipsAccountInBackoff 退避窗口内的账号被跳过并如实说明。
func TestTraeCheckAllSkipsAccountInBackoff(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":0,"message":"success"}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	s.noteTraeBackoff("2081842786229369")

	out, err := s.traeCheckAll(context.Background())
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got := stub.claimCalls.Load(); got != 0 {
		t.Fatalf("退避中不该打 claim，实际 %d 次", got)
	}
	if len(out) != 1 || out[0].Status != CheckinSkipped {
		t.Fatalf("应报告为跳过，实际 %+v", out)
	}
}

// TestTraeCheckAllStatusSaysCheckedInMarksToday status 说已签时必须落「今天已签」缓存。
//
// 这条路径原先只设了内存里的 alreadyN 计数，不记日期——于是同一天稍后再跑，
// 又会去查一次 status（上游已是 9074 的窗口期时，这就是白撞）。
func TestTraeCheckAllStatusSaysCheckedInMarksToday(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody: `{"code":0,"enable":true,"checked_in":true}`,
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	out, err := s.traeCheckAll(context.Background())
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinAlready {
		t.Fatalf("应报告为已签，实际 %+v", out)
	}
	if !traeCheckedInToday(s.traeState("2081842786229369"), time.Now()) {
		t.Fatal("status 说已签后应落「今天已签」缓存（否则同一天还会反复查）")
	}
}

// TestTraeRetryLoopRespectsTaskDisabled 任务被停用后重试循环不再跑。
func TestTraeRetryLoopRespectsTaskDisabled(t *testing.T) {
	stub := &traeCheckinStub{
		statusBody:  `{"code":0,"enable":true,"checked_in":false}`,
		claimBodies: []string{`{"code":9074,"message":"限流"}`, `{"code":0,"message":"success"}`},
	}
	s, _ := newTraeS(t, stub, t.TempDir())
	// 造出「有 9074 痕迹且退避已过期」的状态。
	s.noteTraeBackoff("2081842786229369")
	s.traeCheckin.update("2081842786229369", func(st *traeCheckinState) {
		st.RetryUpdatedAt = time.Now().Add(-time.Hour).Unix()
	})
	if !s.SetTaskEnabled("trae_check", false) {
		t.Fatal("停用 trae_check 应成功")
	}
	// 直接驱动循环体：停用时它应一轮都不做。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	s.traeRetryLoop(ctx)
	if got := stub.claimCalls.Load(); got != 0 {
		t.Fatalf("任务停用后重试循环不该打上游，实际 claim %d 次", got)
	}
}

// TestTraeRetryLoopStopsWithContext 循环随生命周期 ctx 退出（不泄漏 goroutine）。
func TestTraeRetryLoopStopsWithContext(t *testing.T) {
	s := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	s.startTraeRetryLoop(ctx)
	// 重复调用不该起第二个循环。
	s.startTraeRetryLoop(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		s.taskWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后重试循环应退出")
	}
}
