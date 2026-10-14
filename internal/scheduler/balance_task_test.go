// ═══ 更新日志 ═══
// 2026-10-14：新增。锁定「余额刷新与签到解耦」：关掉签到时，余额任务仍必须把
//
//	credits 与 credits_expiring 写进池子，否则「优先消耗快过期积分」静默失效。
package scheduler

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// balanceTestScheduler 搭一个只有余额任务的调度器（签到显式关闭）。
func balanceTestScheduler(t *testing.T, remain int64, cycleEnd string) (*Scheduler, *pool.Pool, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{resourceRemain: remain, cycleEndTime: cycleEnd}
	srv := f.server()
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:         p,
		Upstream:     up,
		BalanceHours: []int{9, 21},
		// 生产由 cmd/server 从 config 的 pool.expiring_soon 传入；这里显式给 7 天，
		// 与线上默认一致（<=0 会禁用分桶，那就测不到本回归了）。
		ExpiringSoonWindow: 7 * 24 * time.Hour,
		CheckinDisabled:    true, // 多客户端共用账号时的典型配置
	})
	return s, p, f
}

// TestBalanceRefreshPopulatesPoolWithoutCheckin 是本轮修复的核心回归。
//
// 背景：credits / credits_expiring 原本只在 checkinAll 里写（SetCreditsDetailed），
// 于是「关掉签到」会连带把这两个字段冻在 0，而 pool 的「付费模型优先消耗快过期
// 积分」硬过滤正依赖 credits_expiring > 0（见 pool/pick.go）。
//
// 线上症状（2026-10-14 行空板实测）：面板显示麋鹿有 1433 快过期积分（面板直查
// 上游），但 /status 里所有 CN 账号 credits 全是 0，选号完全没有快过期偏好。
func TestBalanceRefreshPopulatesPoolWithoutCheckin(t *testing.T) {
	// 到期时间设在窗口内（默认窗口 168h），让余额落进 Expiring 桶。
	end := time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	s, p, f := balanceTestScheduler(t, 1200, end)

	s.RunBalanceNow()

	// 关键断言 1：签到一次都没被调用——本任务必须是纯读的。
	if n := f.checkinCalls.Load(); n != 0 {
		t.Fatalf("balance task must not call daily-checkin, got %d calls", n)
	}
	// 关键断言 2：余额确实写进了池子。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing from pool")
	}
	if st.Credits != 1200 {
		t.Errorf("credits=%d want 1200", st.Credits)
	}
	// 关键断言 3：快过期分桶也写进去了——这正是选号硬过滤的判据。
	// 没有这一条，关掉签到就等于关掉「优先消耗快过期积分」。
	if st.CreditsExpiring != 1200 {
		t.Errorf("credits_expiring=%d want 1200 (the whole balance expires within the window)",
			st.CreditsExpiring)
	}
}

// TestBalanceRefreshKeepsStableCreditsOutOfExpiringBucket 是对侧回归：窗口外的
// 余额不能被误标成快过期，否则选号会无谓地偏向这个号。
func TestBalanceRefreshKeepsStableCreditsOutOfExpiringBucket(t *testing.T) {
	end := time.Now().Add(60 * 24 * time.Hour).Format("2006-01-02 15:04:05") // 60 天 >> 168h 窗口
	s, p, _ := balanceTestScheduler(t, 800, end)

	s.RunBalanceNow()

	st, _ := p.Status("u1")
	if st.Credits != 800 {
		t.Errorf("credits=%d want 800", st.Credits)
	}
	if st.CreditsExpiring != 0 {
		t.Errorf("credits_expiring=%d want 0 (expiry is outside the window)", st.CreditsExpiring)
	}
}

// TestBalanceTaskIsRegisteredAndIndependentlySwitchable 锁定任务注册表：余额任务
// 必须能被面板看到、被单独开关（这正是「方便同时使用多个客户端的用户」的要求）。
func TestBalanceTaskIsRegisteredAndIndependentlySwitchable(t *testing.T) {
	s := New(Config{})
	var found bool
	for _, info := range s.TaskSnapshot() {
		if info.Key != "balance" {
			continue
		}
		found = true
		if info.Name == "" || info.Detail == "" {
			t.Error("balance task must carry a name and detail for the panel")
		}
		if !info.Enabled {
			t.Error("balance task must default to enabled")
		}
		// 它作用在 WorkBuddy 两域（trae 走另一套端点，实现里整条跳过）。
		if len(info.Realms) != 2 || info.Realms[0] != "cn" || info.Realms[1] != "global" {
			t.Errorf("realms=%v want [cn global]", info.Realms)
		}
	}
	if !found {
		t.Fatal("balance task is not registered in the snapshot")
	}

	// 单独关闭余额任务，不得影响签到任务的开关状态。
	off := New(Config{BalanceDisabled: true})
	for _, info := range off.TaskSnapshot() {
		if info.Key == "balance" && info.Enabled {
			t.Error("BalanceDisabled=true must disable the balance task")
		}
		if info.Key == "checkin" && !info.Enabled {
			t.Error("disabling balance must not disable checkin")
		}
	}
}

// TestDisablingCheckinLeavesBalanceTaskRunning 锁定解耦的语义：两个开关互不影响。
// 这是用户要的「任务可以单独关闭，方便同时使用多个客户端的用户」。
func TestDisablingCheckinLeavesBalanceTaskRunning(t *testing.T) {
	s := New(Config{CheckinDisabled: true})
	for _, info := range s.TaskSnapshot() {
		if info.Key == "balance" {
			if !info.Enabled {
				t.Error("disabling checkin must not disable the balance task")
			}
			if info.NextRun == nil {
				t.Error("enabled balance task should still expose a next run time")
			}
		}
	}
}
