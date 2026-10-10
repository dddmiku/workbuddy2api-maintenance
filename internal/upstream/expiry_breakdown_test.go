package upstream

// 2026-10-10：新增到期分档（ExpiryBreakdown）与到期日程（Schedule）用例：
//             各档互斥、合计 = remain、无到期时间落 unlimited、ExpiringWithin 累计口径、
//             日程升序/同刻合并/截断聚尾巴。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestExpiryBreakdownBuckets 锁住固定分档口径：各档互斥、合计 = remain，
// 无到期时间落 unlimited，next_end 取最早到期。
func TestExpiryBreakdownBuckets(t *testing.T) {
	now := time.Now()
	mk := func(d time.Duration) string { return now.Add(d).Format(packageEndLayout) }
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"a","CycleEndTime":"` + mk(12*time.Hour) + `","CycleCapacitySize":10,"CycleCapacityRemain":10},` +
				`{"PackageName":"b","CycleEndTime":"` + mk(2*24*time.Hour) + `","CycleCapacitySize":20,"CycleCapacityRemain":20},` +
				`{"PackageName":"c","CycleEndTime":"` + mk(5*24*time.Hour) + `","CycleCapacitySize":30,"CycleCapacityRemain":30},` +
				`{"PackageName":"d","CycleEndTime":"` + mk(20*24*time.Hour) + `","CycleCapacitySize":40,"CycleCapacityRemain":40},` +
				`{"PackageName":"e","CycleEndTime":"` + mk(90*24*time.Hour) + `","CycleCapacitySize":50,"CycleCapacityRemain":50},` +
				`{"PackageName":"f","CycleCapacitySize":60,"CycleCapacityRemain":60}`), nil
	})
	u, err := c.ResourceUsage(&auth.Auth{AccessToken: "at", UID: "u1"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if u.Remain != 210 {
		t.Errorf("remain=%d want 210", u.Remain)
	}
	b := u.Expiry
	if b.Within1d != 10 {
		t.Errorf("within_1d=%d want 10", b.Within1d)
	}
	if b.Within3d != 20 {
		t.Errorf("within_3d=%d want 20", b.Within3d)
	}
	if b.Within7d != 30 {
		t.Errorf("within_7d=%d want 30", b.Within7d)
	}
	if b.Within30d != 40 {
		t.Errorf("within_30d=%d want 40", b.Within30d)
	}
	if b.Later != 50 {
		t.Errorf("later=%d want 50", b.Later)
	}
	if b.Unlimited != 60 {
		t.Errorf("unlimited=%d want 60", b.Unlimited)
	}
	if sum := b.Within1d + b.Within3d + b.Within7d + b.Within30d + b.Later + b.Unlimited; sum != u.Remain {
		t.Errorf("档位合计=%d != remain=%d（各档必须互斥且覆盖全部余额）", sum, u.Remain)
	}
	if b.NextEnd.IsZero() {
		t.Error("next_end 不应为零值")
	}
	// ExpiringWithin 累计口径。
	if got := b.ExpiringWithin(7 * 24 * time.Hour); got != 60 {
		t.Errorf("ExpiringWithin(7d)=%d want 60 (10+20+30)", got)
	}
	// u.Expiring（窗口分桶）应与 ExpiringWithin 同口径。
	if u.Expiring != 60 {
		t.Errorf("expiring=%d want 60", u.Expiring)
	}
}

// TestExpiryBreakdownExpiringWithin 累计口径：含更紧迫档位、不含 unlimited
// （无到期时间的积分永远不会作废，不该被算成"即将过期"）。
func TestExpiryBreakdownExpiringWithin(t *testing.T) {
	b := ExpiryBreakdown{Within1d: 1, Within3d: 2, Within7d: 3, Within30d: 4, Later: 5, Unlimited: 100}
	if _, err := json.Marshal(b); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cases := []struct {
		d    time.Duration
		want int64
	}{
		{24 * time.Hour, 1},
		{3 * 24 * time.Hour, 3},
		{7 * 24 * time.Hour, 6},
		{30 * 24 * time.Hour, 10},
		{100 * 24 * time.Hour, 15},
	}
	for _, c := range cases {
		if got := b.ExpiringWithin(c.d); got != c.want {
			t.Errorf("ExpiringWithin(%v)=%d want %d", c.d, got, c.want)
		}
	}
}

// TestScheduleSortsAndMerges 日程必须升序且同刻合并：上游返回的套餐顺序不保证有序，
// 同一批赠送包的时间戳还会差几秒（12:00:03 / 12:00:08），不合并会列出几十条同刻条目。
func TestScheduleSortsAndMerges(t *testing.T) {
	now := time.Now()
	mk := func(d time.Duration) string { return now.Add(d).Format(packageEndLayout) }
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 故意乱序，且前两条相差 5 秒（应合并）。
		return mkDetailedResp(
			`{"PackageName":"b","CycleEndTime":"` + mk(5*24*time.Hour) + `","CycleCapacitySize":20,"CycleCapacityRemain":20},` +
				`{"PackageName":"c","CycleEndTime":"` + mk(2*24*time.Hour+5*time.Second) + `","CycleCapacitySize":30,"CycleCapacityRemain":30},` +
				`{"PackageName":"a","CycleEndTime":"` + mk(2*24*time.Hour) + `","CycleCapacitySize":10,"CycleCapacityRemain":10},` +
				`{"PackageName":"d","CycleEndTime":"` + mk(20*time.Hour) + `","CycleCapacitySize":40,"CycleCapacityRemain":40}`), nil
	})
	u, err := c.ResourceUsage(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(u.Schedule) != 3 {
		t.Fatalf("schedule=%d 条 want 3（4 个套餐、其中 2 个同刻应合并）: %+v", len(u.Schedule), u.Schedule)
	}
	for i := 1; i < len(u.Schedule); i++ {
		if u.Schedule[i].End.Before(u.Schedule[i-1].End) {
			t.Errorf("日程未升序: %v", u.Schedule)
		}
	}
	// 最近的一笔 = 20 小时后的 40。
	if !u.Schedule[0].End.Equal(now.Add(20 * time.Hour).Truncate(time.Minute)) {
		t.Errorf("首条 End=%v want 20h 后", u.Schedule[0].End)
	}
	if u.Schedule[0].Amount != 40 {
		t.Errorf("首条 Amount=%d want 40", u.Schedule[0].Amount)
	}
	// 合并的那笔：10+30=40，套餐数 2。
	if u.Schedule[1].Amount != 40 || u.Schedule[1].Packages != 2 {
		t.Errorf("合并条目=%+v want {Amount:40 Packages:2}", u.Schedule[1])
	}
	if u.Schedule[2].Amount != 20 {
		t.Errorf("末条 Amount=%d want 20", u.Schedule[2].Amount)
	}
}

// TestScheduleTotalEqualsRemain 日程合计必须等于"有到期时间的余额"：
// 截断时余额聚成尾巴而不是丢弃，否则面板会低报要作废的积分。
func TestScheduleTotalEqualsRemain(t *testing.T) {
	now := time.Now()
	var body string
	want := int64(0)
	// 造 60 个不同到期时刻（超过 maxScheduleEntries=40），每笔余额 i+1。
	for i := 0; i < 60; i++ {
		if i > 0 {
			body += ","
		}
		end := now.Add(time.Duration(i+1) * 24 * time.Hour).Format(packageEndLayout)
		body += `{"PackageName":"p","CycleEndTime":"` + end + `","CycleCapacitySize":100,"CycleCapacityRemain":` +
			itoa(int64(i+1)) + `}`
		want += int64(i + 1)
	}
	c := testClient(func(r *http.Request) (*http.Response, error) { return mkDetailedResp(body), nil })
	u, err := c.ResourceUsage(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !u.ScheduleTruncated {
		t.Error("60 条应标记 truncated")
	}
	if len(u.Schedule) != maxScheduleEntries {
		t.Errorf("schedule=%d 条 want %d", len(u.Schedule), maxScheduleEntries)
	}
	var sum int64
	for _, e := range u.Schedule {
		sum += e.Amount
	}
	if sum != want {
		t.Errorf("日程合计=%d want %d（截断必须聚成尾巴，不能丢余额）", sum, want)
	}
	if sum != u.Remain {
		t.Errorf("日程合计=%d != remain=%d", sum, u.Remain)
	}
}

// TestScheduleSkipsNoEndTime 无到期时间的套餐不进日程（它们不会作废，
// 列进去会让"什么时候过期多少"变成误导），但要计入 Unlimited 与 remain。
func TestScheduleSkipsNoEndTime(t *testing.T) {
	now := time.Now()
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"a","CycleEndTime":"` + now.Add(3*24*time.Hour).Format(packageEndLayout) + `","CycleCapacitySize":100,"CycleCapacityRemain":60},` +
				`{"PackageName":"forever","CycleCapacitySize":100,"CycleCapacityRemain":40}`), nil
	})
	u, err := c.ResourceUsage(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if len(u.Schedule) != 1 || u.Schedule[0].Amount != 60 {
		t.Errorf("schedule=%+v want 单条 60", u.Schedule)
	}
	if u.Expiry.Unlimited != 40 {
		t.Errorf("unlimited=%d want 40", u.Expiry.Unlimited)
	}
	if u.Remain != 100 {
		t.Errorf("remain=%d want 100", u.Remain)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
