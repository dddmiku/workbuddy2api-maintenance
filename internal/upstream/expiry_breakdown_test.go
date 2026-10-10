package upstream

// 2026-10-10：新增到期分档（ExpiryBreakdown）用例：各档互斥、合计 = remain、
//             无到期时间落 unlimited、ExpiringWithin 累计口径。

import (
	"encoding/json"
	"net/http"
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
