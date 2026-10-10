package upstream

// 2026-10-10：到期字段 fixture 改用真实的 CycleEndTime（此前误用 PackageEndTime，
//             测试全绿而线上分桶恒为 0）；新增生产线形与兜底键用例。

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// mkDetailedResp 构造带到期时间的 get-user-resource 响应。
//
// 注意：到期字段是 **CycleEndTime**，不是 PackageEndTime——这是 2026-10-10 从生产
// 抓的真实响应里核实的（CN 119 包 + global 19 包，PackageEndTime 出现 0 次）。
// 本文件原先的 fixture 用了 PackageEndTime，于是测试全绿而线上分桶恒为 0
// （见 client.go 到期字段名注释）。fixture 必须与线上线形一致。
func mkDetailedResp(accounts string) *http.Response {
	body := `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[` + accounts + `]}}}}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Header:     make(http.Header),
	}
}

func TestUserResourceDetailedSplitsExpiring(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, buckets, err := c.UserResourceDetailed(a, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if buckets.Expiring != 1200 {
		t.Errorf("expiring=%d want 1200 (3天内过期的奖励包)", buckets.Expiring)
	}
	if buckets.Stable != 300 {
		t.Errorf("stable=%d want 300 (30天后才过期的周期包)", buckets.Stable)
	}
	if buckets.Total() != remain {
		t.Errorf("total=%d != remain=%d", buckets.Total(), remain)
	}
}

// TestUserResourceDetailedLegacyPackageEndTime 兜底键回归：上游若改回/双写
// PackageEndTime，仍能正确分桶（不因改名静默失效）。
func TestUserResourceDetailedLegacyPackageEndTime(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","PackageEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 80 {
		t.Errorf("expiring=%d want 80 (PackageEndTime 兜底键)", buckets.Expiring)
	}
}

// TestUserResourceDetailedPrefersCycleEndTime 两个键同时存在时以 CycleEndTime 为准：
// 它在窗口内、PackageEndTime 在窗口外 → 必须计入 Expiring（不能因兜底键把
// 真正快到期的余额判成 Stable）。
func TestUserResourceDetailedPrefersCycleEndTime(t *testing.T) {
	now := time.Now()
	in2d := now.Add(2 * 24 * time.Hour).Format(packageEndLayout)
	in60d := now.Add(60 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in2d + `","PackageEndTime":"` + in60d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 80 || buckets.Stable != 0 {
		t.Errorf("buckets=%+v want {Expiring:80 Stable:0} (CycleEndTime 优先)", buckets)
	}
}

// TestUserResourceDetailedRealWireShape 用生产抓包的真实字段名做端到端分桶：
// CycleEndTime 是上游唯一实际发出的到期字段，这条用例锁住「线上真的能分出来」。
// 数据取自 2026-10-10 dump 的 CN 账号（套餐名/字段名/时间格式逐字）。
func TestUserResourceDetailedRealWireShape(t *testing.T) {
	now := time.Now()
	soon := now.Add(7 * 24 * time.Hour).Format(packageEndLayout) // 窗口内
	far := now.Add(40 * 24 * time.Hour).Format(packageEndLayout) // 窗口外
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"CodeBuddy个人版赠送运营费包","PackageCode":"TCACA_code_007_nzdH5h4Nl0",` +
				`"CycleStartTime":"2026-09-15 11:09:28","CycleEndTime":"` + soon + `",` +
				`"CapacityRemain":710,"CapacitySize":1500,"CycleCapacityRemain":710,"CycleCapacitySize":1500,"CycleCapacityUsed":789,"Status":0},` +
				`{"PackageName":"CodeBuddy个人版订阅","PackageCode":"TCACA_code_008_cfWoLwvjU4",` +
				`"CycleStartTime":"2026-10-01 00:00:00","CycleEndTime":"` + far + `",` +
				`"CapacityRemain":500,"CapacitySize":500,"CycleCapacityRemain":500,"CycleCapacitySize":500,"Status":0}`), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at", UID: "4e183777"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1210 {
		t.Errorf("remain=%d want 1210", remain)
	}
	if buckets.Expiring != 710 {
		t.Errorf("expiring=%d want 710 (运营费包在 7 天窗口内)", buckets.Expiring)
	}
	if buckets.Stable != 500 {
		t.Errorf("stable=%d want 500 (订阅包 40 天后到期)", buckets.Stable)
	}
}

func TestUserResourceDetailedNoWindowAllStable(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	// soon<=0：禁用分桶，全部归 Stable（向后兼容旧行为）。
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} when soon=0", buckets)
	}
}

func TestUserResourceDetailedMissingEndTimeStable(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 无任何到期时间字段：保守归 Stable，不误标快过期插队。
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} for missing end time", buckets)
	}
}

// TestUserResourceDetailedBadEndTimeStable 到期时间无法解析时保守归 Stable。
func TestUserResourceDetailedBadEndTimeStable(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"not-a-time","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} for unparsable end time", buckets)
	}
}

func TestUserResourceBackwardCompat(t *testing.T) {
	// 旧 UserResource 签名与总量口径不变。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 80 {
		t.Errorf("UserResource remain=%d err=%v, want 80/nil", remain, err)
	}
}
