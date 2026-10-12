// ═══ 更新日志 ═══
// 2026-10-13：初版——登录限流与真实客户端 IP 的 Go 版（与 panel/app.py 同口径）。
//
// 这些用例锁住的是「取错就会出事」的几条规则：转发头只在可信对端下采信、IPv6 按 /64
// 归桶、空桶必须删掉、桶表超限按 1/8 淘汰。时钟全部注入，断言精确到秒，不依赖真实时间。
package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// throttleClock 是注入给 Throttle 的假时钟。
type throttleClock struct{ at time.Time }

func (c *throttleClock) Now() time.Time          { return c.at }
func (c *throttleClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// newTestThrottle 构造一个用假时钟驱动的限流器。
func newTestThrottle(trustedProxies, adminAllowCIDRs string) (*Throttle, *throttleClock) {
	clock := &throttleClock{at: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	throttle := NewThrottle(trustedProxies, adminAllowCIDRs)
	throttle.now = clock.Now
	return throttle, clock
}

// throttleRequest 造一个指定直连对端与请求头的请求。
func throttleRequest(remoteAddr string, headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/admin/login", nil)
	request.RemoteAddr = remoteAddr
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return request
}

// 直连对端不是可信代理时，转发头必须被完全忽略——否则轮换 X-Real-IP 就能换桶。
func TestClientIPUntrustedPeerIgnoresForwardingHeaders(t *testing.T) {
	throttle, _ := newTestThrottle(TrustedProxiesRaw, "")
	request := throttleRequest("203.0.113.9:5555", map[string]string{
		"X-Real-IP":        "1.1.1.1",
		"CF-Connecting-IP": "2.2.2.2",
		"X-Forwarded-For":  "3.3.3.3, 4.4.4.4",
	})
	if got := throttle.ClientIP(request); got != "203.0.113.9" {
		t.Fatalf("不可信对端的转发头必须被忽略，得到 %q，期望 %q", got, "203.0.113.9")
	}
}

// 可信对端下按 X-Real-IP → CF-Connecting-IP → X-Forwarded-For 的优先级取第一个非空头，
// 且只取第一个逗号分隔元素。
func TestClientIPTrustedPeerHeaderPrecedence(t *testing.T) {
	throttle, _ := newTestThrottle(TrustedProxiesRaw, "")
	cases := []struct {
		name     string
		headers  map[string]string
		expected string
	}{
		{
			name: "三个头都在时取 X-Real-IP",
			headers: map[string]string{
				"X-Real-IP":        "1.1.1.1",
				"CF-Connecting-IP": "2.2.2.2",
				"X-Forwarded-For":  "3.3.3.3",
			},
			expected: "1.1.1.1",
		},
		{
			name:     "只有 CF-Connecting-IP 时取它",
			headers:  map[string]string{"CF-Connecting-IP": "2.2.2.2"},
			expected: "2.2.2.2",
		},
		{
			name:     "只有 X-Forwarded-For 时取第一个逗号元素并去空白",
			headers:  map[string]string{"X-Forwarded-For": "3.3.3.3, 4.4.4.4"},
			expected: "3.3.3.3",
		},
		{
			name:     "X-Real-IP 为空串时回落到下一个头",
			headers:  map[string]string{"X-Real-IP": "  ", "CF-Connecting-IP": "2.2.2.2"},
			expected: "2.2.2.2",
		},
		{
			name:     "没有任何转发头时用直连对端",
			headers:  nil,
			expected: "127.0.0.1",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := throttleRequest("127.0.0.1:43210", testCase.headers)
			if got := throttle.ClientIP(request); got != testCase.expected {
				t.Fatalf("ClientIP = %q，期望 %q", got, testCase.expected)
			}
		})
	}
}

// 空对端地址统一成 "?"。
func TestClientIPEmptyPeerIsQuestionMark(t *testing.T) {
	throttle, _ := newTestThrottle(TrustedProxiesRaw, "")
	if got := throttle.ClientIP(throttleRequest("", nil)); got != "?" {
		t.Fatalf("空对端应返回 %q，得到 %q", "?", got)
	}
}

// IPv6 必须归并到 /64，且字符串形态与 Python 的 ip_network(..., strict=False) 完全一致。
func TestBucketKeyIPv6Slash64(t *testing.T) {
	cases := []struct{ in, want string }{
		// 关键断言：与 str(ipaddress.ip_network("2001:db8::1/64", strict=False)) 逐字相同。
		{"2001:db8::1", "2001:db8::/64"},
		{"2001:db8:1:2:3:4:5:6", "2001:db8:1:2::/64"},
		{"2001:db8::", "2001:db8::/64"},
		{"::1", "::/64"},
		// IPv4 原样。
		{"1.2.3.4", "1.2.3.4"},
		{" 10.0.0.1 ", "10.0.0.1"},
		// IPv4-mapped 在 Python 里 version 仍是 6，归并成 "::/64"（不是 IPv4）——同口径。
		{"::ffff:1.2.3.4", "::/64"},
	}
	for _, testCase := range cases {
		if got := bucketKey(testCase.in); got != testCase.want {
			t.Fatalf("bucketKey(%q) = %q，期望 %q", testCase.in, got, testCase.want)
		}
	}
}

// 无法解析的输入原样作为键（不做 trim，与 Python 的 str(ip) 一致）。
func TestBucketKeyUnparseablePassThrough(t *testing.T) {
	for _, raw := range []string{"", "not-an-ip", "1.2.3.4/24", "999.1.1.1", "some-host"} {
		if got := bucketKey(raw); got != raw {
			t.Fatalf("bucketKey(%q) = %q，期望原样返回", raw, got)
		}
	}
}

// 管理入口白名单：未配置放行一切；配置后只放行网段内地址，回环始终放行，非法地址拒绝。
func TestAdminClientAllowedCIDRAndLoopback(t *testing.T) {
	t.Run("未配置白名单则全部放行", func(t *testing.T) {
		throttle, _ := newTestThrottle(TrustedProxiesRaw, "")
		for _, remote := range []string{"203.0.113.9:1", "127.0.0.1:1", ""} {
			if !throttle.AdminClientAllowed(throttleRequest(remote, nil)) {
				t.Fatalf("未配置白名单时 %q 应放行", remote)
			}
		}
	})

	t.Run("配置后按网段放行", func(t *testing.T) {
		// 可信代理留空，直连对端即客户端 IP，便于精确控制。
		throttle, _ := newTestThrottle("", "203.0.113.0/24, 198.51.100.7")
		allowed := []string{"203.0.113.0:1", "203.0.113.255:1", "198.51.100.7:1"}
		denied := []string{"198.51.100.8:1", "10.0.0.1:1", "2001:db8::1:1"}
		for _, remote := range allowed {
			if !throttle.AdminClientAllowed(throttleRequest(remote, nil)) {
				t.Fatalf("%q 在白名单内应放行", remote)
			}
		}
		for _, remote := range denied {
			if throttle.AdminClientAllowed(throttleRequest(remote, nil)) {
				t.Fatalf("%q 不在白名单内应拒绝", remote)
			}
		}
	})

	t.Run("回环始终放行", func(t *testing.T) {
		throttle, _ := newTestThrottle("", "203.0.113.0/24")
		for _, remote := range []string{"127.0.0.1:1", "[::1]:1"} {
			if !throttle.AdminClientAllowed(throttleRequest(remote, nil)) {
				t.Fatalf("回环 %q 应始终放行（就绪探针与本地排障）", remote)
			}
		}
	})

	t.Run("无法解析的客户端地址拒绝", func(t *testing.T) {
		throttle, _ := newTestThrottle("", "203.0.113.0/24")
		if throttle.AdminClientAllowed(throttleRequest("", nil)) {
			t.Fatal("客户端 IP 为 \"?\" 时应拒绝")
		}
	})

	t.Run("非法白名单项被跳过且不影响启动", func(t *testing.T) {
		throttle, _ := newTestThrottle("", "not-a-cidr,203.0.113.0/24")
		if len(throttle.adminAllow) != 1 {
			t.Fatalf("非法项应被跳过，剩余 %d 条", len(throttle.adminAllow))
		}
		if !throttle.AdminClientAllowed(throttleRequest("203.0.113.5:1", nil)) {
			t.Fatal("合法网段应仍然生效")
		}
	})
}

// 带主机位的网段按 strict=False 语义掩码（10.0.0.1/8 等价于 10.0.0.0/8）。
func TestTrustedProxyHostBitsAreMasked(t *testing.T) {
	throttle, _ := newTestThrottle("10.0.0.1/8", "")
	if len(throttle.trustedProxies) != 1 {
		t.Fatalf("应解析出 1 个网段，得到 %d", len(throttle.trustedProxies))
	}
	if got := throttle.trustedProxies[0].String(); got != "10.0.0.0/8" {
		t.Fatalf("网段应被掩码为 10.0.0.0/8，得到 %q", got)
	}
	if !throttle.isTrustedProxy("10.9.9.9") {
		t.Fatal("10.9.9.9 应落在掩码后的 10.0.0.0/8 内")
	}
	// 版本不匹配不得命中。
	if throttle.isTrustedProxy("2001:db8::1") {
		t.Fatal("IPv6 地址不应命中 IPv4 网段")
	}
	if throttle.isTrustedProxy("") || throttle.isTrustedProxy("garbage") {
		t.Fatal("空/非法地址一律按不可信处理")
	}
}

// 单 IP 阈值：窗口内第 6 次失败触发，窗口过期后清零。
func TestLoginBlockedThresholdAndWindow(t *testing.T) {
	throttle, clock := newTestThrottle(TrustedProxiesRaw, "")
	const ip = "203.0.113.77"

	for i := 0; i < LoginMaxFails-1; i++ {
		blocked, hits := throttle.BlockedAndRecord(ip)
		if blocked {
			t.Fatalf("第 %d 次失败不该被挡住（hits=%d）", i+1, hits)
		}
		if hits != i {
			t.Fatalf("第 %d 次失败前 hits 应为 %d，得到 %d", i+1, i, hits)
		}
	}
	// 第 6 次：记账后达到阈值。
	if blocked, _ := throttle.BlockedAndRecord(ip); blocked {
		t.Fatal("第 6 次失败本身不该被挡住")
	}
	if blocked, hits := throttle.Blocked(ip); !blocked || hits != LoginMaxFails {
		t.Fatalf("第 6 次失败后应被挡住且 hits=%d，得到 blocked=%v hits=%d", LoginMaxFails, blocked, hits)
	}
	if retry := throttle.GlobalRetryAfter(); retry != 0 {
		t.Fatalf("6 次失败不该触发全站节流，得到 %d", retry)
	}

	// 窗口内再失败仍被挡住。
	clock.advance(LoginWindow - time.Second)
	if blocked, _ := throttle.Blocked(ip); !blocked {
		t.Fatal("窗口内应持续被挡住")
	}

	// 越过窗口：记录全部过期，桶被清掉。
	clock.advance(2 * time.Second)
	blocked, hits := throttle.Blocked(ip)
	if blocked || hits != 0 {
		t.Fatalf("窗口过期后应解封，得到 blocked=%v hits=%d", blocked, hits)
	}
}

// Blocked 只读不写：查询本身绝不能记账，否则每个请求都会给自己加一次失败。
func TestLoginBlockedDoesNotRecord(t *testing.T) {
	throttle, _ := newTestThrottle(TrustedProxiesRaw, "")
	const ip = "203.0.113.88"
	for i := 0; i < 10; i++ {
		if blocked, hits := throttle.Blocked(ip); blocked || hits != 0 {
			t.Fatalf("第 %d 次查询不该记账，得到 blocked=%v hits=%d", i+1, blocked, hits)
		}
	}
	if len(throttle.fails) != 0 {
		t.Fatalf("查询不该留下空桶，桶表大小 %d", len(throttle.fails))
	}
	throttle.Failed(ip)
	if blocked, hits := throttle.Blocked(ip); blocked || hits != 1 {
		t.Fatalf("记一次失败后应为 hits=1，得到 blocked=%v hits=%d", blocked, hits)
	}
}

// 全局节流：窗口内累计 20 次失败后拒绝新尝试，返回剩余等待秒数；login_ok 清空全站时间线。
func TestLoginGlobalThrottleAndOKClears(t *testing.T) {
	throttle, clock := newTestThrottle(TrustedProxiesRaw, "")

	// 19 次失败（分散在不同 IP，避免触发单 IP 阈值）仍可继续。
	for i := 0; i < LoginGlobalMaxFails-1; i++ {
		throttle.Failed(fmt.Sprintf("10.0.0.%d", i+1))
	}
	if retry := throttle.GlobalRetryAfter(); retry != 0 {
		t.Fatalf("%d 次失败不该触发全局节流，得到 %d", LoginGlobalMaxFails-1, retry)
	}

	// 第 20 次触发；所有失败发生在同一时刻，剩余窗口 = 60s，retry = int(60)+1 = 61。
	throttle.Failed("10.0.0.20")
	if retry := throttle.GlobalRetryAfter(); retry != 61 {
		t.Fatalf("刚触发时 retry 应为 61，得到 %d", retry)
	}

	// 时间推进 1 秒：最旧记录已过 1 秒，retry = int(59)+1 = 60。
	clock.advance(time.Second)
	if retry := throttle.GlobalRetryAfter(); retry != 60 {
		t.Fatalf("推进 1 秒后 retry 应为 60，得到 %d", retry)
	}

	// 管理员成功登录：全站节流立即解除，真人到场不该被别人的失败连坐。
	throttle.OK("10.0.0.1")
	if retry := throttle.GlobalRetryAfter(); retry != 0 {
		t.Fatalf("OK 后应解除全站节流，得到 %d", retry)
	}
	if len(throttle.globalFails) != 0 {
		t.Fatalf("OK 应清空全局时间线，剩余 %d 条", len(throttle.globalFails))
	}

	// 窗口过期同样自动解除。
	for i := 0; i < LoginGlobalMaxFails; i++ {
		throttle.Failed(fmt.Sprintf("10.1.0.%d", i+1))
	}
	if retry := throttle.GlobalRetryAfter(); retry == 0 {
		t.Fatal("重新累计到上限后应再次节流")
	}
	clock.advance(LoginGlobalWindow + time.Second)
	if retry := throttle.GlobalRetryAfter(); retry != 0 {
		t.Fatalf("窗口过期后应解除节流，得到 %d", retry)
	}
}

// 空桶必须删除：否则未鉴权的登录端点能把分桶表撑到任意大小。
func TestLoginEmptyBucketDeleted(t *testing.T) {
	throttle, clock := newTestThrottle(TrustedProxiesRaw, "")
	const ip = "203.0.113.99"

	throttle.Failed(ip)
	if _, exists := throttle.fails[bucketKey(ip)]; !exists {
		t.Fatal("失败后应存在分桶")
	}
	clock.advance(LoginWindow + time.Second)
	if blocked, hits := throttle.Blocked(ip); blocked || hits != 0 {
		t.Fatalf("窗口过期后应为 blocked=false hits=0，得到 %v %d", blocked, hits)
	}
	if _, exists := throttle.fails[bucketKey(ip)]; exists {
		t.Fatal("窗口内无失败记录时必须删除空桶，而不是留下空切片")
	}

	// 从未出现过的 IP 被查询也不该产生桶。
	if _, _ = throttle.Blocked("198.51.100.123"); len(throttle.fails) != 0 {
		t.Fatalf("查询未知 IP 不该创建桶，桶表大小 %d", len(throttle.fails))
	}
}

// 桶表超限时按「最后失败时间」一次淘汰 1/8，最旧的一批先走。
func TestLoginBucketTableEviction(t *testing.T) {
	throttle, clock := newTestThrottle(TrustedProxiesRaw, "")
	base := clock.at

	// 预置 10000 个桶，i 越大越新。
	for i := 0; i < LoginBucketLimit; i++ {
		key := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		throttle.fails[key] = []time.Time{base.Add(time.Duration(i) * time.Second)}
	}

	// 第一次失败：桶数正好等于上限，trim 是空操作，随后插入新桶 → 10001。
	clock.advance(20000 * time.Second)
	throttle.Failed("new-ip")
	if len(throttle.fails) != LoginBucketLimit+1 {
		t.Fatalf("第一次失败后桶数应为 %d，得到 %d", LoginBucketLimit+1, len(throttle.fails))
	}

	// 第二次失败：超过上限，淘汰最旧的 1/8（10000/8 = 1250）。
	throttle.Failed("new-ip")
	drop := LoginBucketLimit / 8
	want := LoginBucketLimit + 1 - drop
	if len(throttle.fails) != want {
		t.Fatalf("淘汰后桶数应为 %d，得到 %d", want, len(throttle.fails))
	}
	// 最旧的 drop 个（i < 1250）应被清掉，其余保留；最新的 new-ip 一定活着。
	for i := 0; i < drop; i++ {
		key := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		if _, exists := throttle.fails[key]; exists {
			t.Fatalf("最旧的桶 %q（i=%d）应被淘汰", key, i)
		}
	}
	if _, exists := throttle.fails["new-ip"]; !exists {
		t.Fatal("最新的桶不该被淘汰")
	}
}
