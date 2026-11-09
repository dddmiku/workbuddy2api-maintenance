// Trae 快过期积分必须真的写进池子（2026-11-09）。
//
// ═══ 用户需求 ═══
//
// 「做，按照 cn 一样，优先使用过期积分」——Trae 账号此前只调 SetCredits(remain)，
// 从不写 creditsExpiring，选号侧的整套「快过期优先」机制（pick 的
// paid_expiring_preferred 硬过滤 + expiringDeferred 粘性让路）对该域完全失效。
//
// ═══ 为什么必须端到端测 ═══
//
// 只测 trae.ExpiringWithin 这个纯函数，无法发现「函数写对了但调用点没接上」。
// 本文件走 traeCheckAll 的完整链路（含 FetchCredits 打假上游 → 解析 → 写池），
// 断言池子里 creditsExpiring 真的是非零。
//
// 与 WorkBuddy 侧 balance_task_test.go 是同一类回归：那边曾因「关掉签到就把
// credits_expiring 冻在 0」导致特性静默失效。
package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream/trae"
)

// traeCreditsStub 假 Trae 上游：ent_usage 返回两个包，一个在窗口内到期。
//
// 结构照抄 2026-11-09 用 curl 抓到的真实响应（剩余量在 quota − usage.credits_amount，
// 不是 remaining_amount）。
func traeCreditsStub(t *testing.T, soonPackExpire int64, laterPackExpire int64) {
	t.Helper()
	body := `{"is_credits_billing":true,` +
		`"usage_summary":{"consumed_amount":4129.12,"total_amount":6800},` +
		`"user_entitlement_pack_list":[` +
		// 包 1：额度 1000，已用 600 → 剩 400，窗口内到期
		`{"display_desc":"福利积分","expire_time":` + itoa(soonPackExpire) + `,` +
		`"entitlement_base_info":{"end_time":` + itoa(soonPackExpire) + `,` +
		`"product_extra":{"package_extra":{"package_name":"福利积分",` +
		`"quota":{"credits_limit":1000}}}},` +
		`"usage":{"credits_amount":600}},` +
		// 包 2：额度 2000，已用 0 → 剩 2000，窗口外到期
		`{"display_desc":"长期积分","expire_time":` + itoa(laterPackExpire) + `,` +
		`"entitlement_base_info":{"end_time":` + itoa(laterPackExpire) + `,` +
		`"product_extra":{"package_extra":{"package_name":"长期积分",` +
		`"quota":{"credits_limit":2000}}}},` +
		`"usage":{}}` +
		`]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	old := trae.TraeAPIHost
	trae.TraeAPIHost = srv.URL
	t.Cleanup(func() {
		trae.TraeAPIHost = old
		srv.Close()
	})
}


// newTraeAuthForTest 走 auth.Parse 的嵌套形构造 trae 账号。
//
// realm 是未导出字段，Realm() 靠它判定；手搓 Auth 结构会让 a.Realm() 返回 cn，
// traeCheckAll 的「只处理 trae 域」分流就永远不命中（这个坑踩过一次）。
func newTraeAuthForTest(t *testing.T, uid string) *auth.Auth {
	t.Helper()
	raw := []byte(`{"account":{"uid":"` + uid + `","nickname":"t"},` +
		`"auth":{"accessToken":"tok","refreshToken":"refresh",` +
		`"expiresAt":4102444800,"realm":"trae"}}`)
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("构造 trae 账号失败：%v", err)
	}
	a.FilePath = filepath.Join(t.TempDir(), "a.json")
	return a
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// 核心回归：Trae 签到时必须把快过期额度写进池子。
func TestTraeCheckinPopulatesExpiringCredits(t *testing.T) {
	now := time.Now()
	// 一个 3 天后到期（在默认 7 天窗口内），一个 60 天后到期（窗口外）。
	traeCreditsStub(t, now.Add(72*time.Hour).Unix(), now.Add(60*24*time.Hour).Unix())

	p := pool.New("")
	p.Add(newTraeAuthForTest(t, "trae-uid-0001"))
	s := New(Config{
		Pool:               p,
		ExpiringSoonWindow: 7 * 24 * time.Hour,
		TraeCheckHours:     []int{11},
		CheckinDisabled:    true,
	})

	if _, err := s.traeCheckAll(t.Context()); err != nil {
		t.Fatalf("traeCheckAll: %v", err)
	}

	st, ok := p.Status("trae-uid-0001")
	if !ok {
		t.Fatal("账号不在池子里")
	}
	// remain = 6800 − 4129.12 ≈ 2671（上游汇总口径）
	if st.Credits <= 0 {
		t.Fatalf("Credits = %d，余额没写进池子", st.Credits)
	}
	// 关键断言：窗口内那个包的剩余 400 必须记进 creditsExpiring。
	if st.CreditsExpiring != 400 {
		t.Fatalf("CreditsExpiring = %d want 400 —— 快过期积分没写进池子，"+
			"选号侧的付费模型优先过滤（paid_expiring_preferred）对该域失效",
			st.CreditsExpiring)
	}
}

// 窗口配置必须生效：同一个响应，窗口放大到 90 天后两个包都算快过期。
func TestTraeCheckinRespectsExpiringWindow(t *testing.T) {
	now := time.Now()
	traeCreditsStub(t, now.Add(72*time.Hour).Unix(), now.Add(60*24*time.Hour).Unix())

	p := pool.New("")
	p.Add(newTraeAuthForTest(t, "trae-uid-0002"))
	s := New(Config{
		Pool:               p,
		ExpiringSoonWindow: 90 * 24 * time.Hour, // 放大到包含 60 天那个包
		TraeCheckHours:     []int{11},
		CheckinDisabled:    true,
	})
	if _, err := s.traeCheckAll(t.Context()); err != nil {
		t.Fatalf("traeCheckAll: %v", err)
	}

	st, _ := p.Status("trae-uid-0002")
	if st.CreditsExpiring != 2400 {
		t.Fatalf("CreditsExpiring = %d want 2400（400 + 2000，窗口 90 天两个包都算）",
			st.CreditsExpiring)
	}
}

// 窗口关闭（<=0）时不分桶，creditsExpiring 保持 0（与 WorkBuddy 侧同口径）。
func TestTraeCheckinZeroWindowDisablesBucketing(t *testing.T) {
	now := time.Now()
	traeCreditsStub(t, now.Add(time.Hour).Unix(), now.Add(60*24*time.Hour).Unix())

	p := pool.New("")
	p.Add(newTraeAuthForTest(t, "trae-uid-0003"))
	s := New(Config{
		Pool:               p,
		ExpiringSoonWindow: 0, // 显式关闭
		TraeCheckHours:     []int{11},
		CheckinDisabled:    true,
	})
	if _, err := s.traeCheckAll(t.Context()); err != nil {
		t.Fatalf("traeCheckAll: %v", err)
	}

	st, _ := p.Status("trae-uid-0003")
	if st.CreditsExpiring != 0 {
		t.Fatalf("窗口为 0 时不该分桶，got %d", st.CreditsExpiring)
	}
}
