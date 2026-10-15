// ═══ 更新日志 ═══
// 2026-10-13：初版——登录限流与真实客户端 IP 的 Go 版（与 panel/app.py 同口径）。
//
// 这一层是安全敏感代码：登录限流按「客户端 IP」分桶，分桶键取错，要么把不相干的
// 人关进同一个小黑屋（误伤），要么让攻击者每次请求换一个桶（限流失效）。所以下面
// 每条不显然的规则都带「为什么」，改动前请先读懂理由。
package panel

import (
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// 与 panel/app.py 逐字同口径的常量。
const (
	// LoginMaxFails 同一 IP 在窗口内的失败次数上限。
	LoginMaxFails = 6
	// LoginWindow 单 IP 限流窗口：300 秒。
	LoginWindow = 300 * time.Second
	// LoginBucketLimit 分桶表的容量上限。
	//
	// 分桶键来自转发头，未鉴权的攻击者能造出任意多的键，没有上限就是一条内存增长面；
	// 超过后清掉最旧的一批，宁可短暂放宽限流也不被撑爆。
	LoginBucketLimit = 10000
	// LoginGlobalWindow 全局节流窗口：60 秒。
	LoginGlobalWindow = 60 * time.Second
	// LoginGlobalMaxFails 全局窗口内的失败次数上限。
	//
	// 单 IP 分桶挡不住轮换 IP 的分布式爆破（IPv6 一个 /64 就有海量地址）。全站失败次数
	// 在窗口内超过上限时，短暂拒绝新的登录尝试——这是节流而不是封禁：窗口只有 1 分钟，
	// 上限远高于管理员自己会失败的次数，避免被用来把管理员锁在门外。
	LoginGlobalMaxFails = 20
	// TrustedProxiesRaw 默认可信反向代理网段（逗号分隔 CIDR）。
	//
	// 只有直连对端落在这些网段内，才采信 X-Real-IP / CF-Connecting-IP / X-Forwarded-For
	// ——这些头客户端能自己伪造，不做来源校验就等于把限流分桶键交给攻击者（轮换一个头
	// 就换一个桶）。默认覆盖回环与私网：文档推荐的部署是「nginx 在同一台机器或同一个
	// Compose 网络里转发到面板」，那种形态下对端必然是回环或容器网段。
	TrustedProxiesRaw = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"
	// AdminAllowCIDRs 可选的管理入口白名单（逗号分隔 CIDR，留空 = 不限制）。
	AdminAllowCIDRs = ""
)

// Throttle 保存登录限流状态与 IP 解析配置。
//
// 全部可变状态由一把锁保护：分桶表、全局失败时间线。读侧（ClientIP /
// AdminClientAllowed / GlobalRetryAfter / Blocked）与写侧（Failed / OK）共用同一把锁，
// 与 Python 版 `_cred_lock` 的行为一致。
type Throttle struct {
	mu sync.Mutex
	// fails 是「分桶键 → 该桶内的失败时间戳」。键由 bucketKey 生成（IPv6 归并到 /64）。
	fails map[string][]time.Time
	// globalFails 是全站失败时间线（升序追加）。
	globalFails []time.Time
	// trustedProxies / adminAllow 在构造时解析一次，之后只读。
	trustedProxies []netip.Prefix
	adminAllow     []netip.Prefix
	// now 可注入，测试用假时钟保证确定性；生产默认 time.Now。
	now func() time.Time
}

// NewThrottle 解析可信代理网段与管理白名单，构造限流器。
//
// trustedProxies / adminAllowCIDRs 是逗号分隔的 CIDR 串，空串表示「无」（可信代理为空
// 则所有转发头都不采信；管理白名单为空则不做入口限制）。非法项跳过并打日志，不影响启动。
func NewThrottle(trustedProxies, adminAllowCIDRs string) *Throttle {
	return &Throttle{
		fails:          make(map[string][]time.Time),
		trustedProxies: parseNetworks(trustedProxies, "忽略无法解析的可信代理网段: %q"),
		adminAllow:     parseNetworks(adminAllowCIDRs, "忽略无法解析的管理白名单网段: %q"),
		now:            time.Now,
	}
}

// parseNetworks 解析逗号分隔的 CIDR 串；非法项跳过并打日志。
//
// 与 Python 的 ipaddress.ip_network(item, strict=False) 对齐：
//   - 带主机位的网段（如 10.0.0.1/8）合法，会被掩码成 10.0.0.0/8；
//   - 只给地址不给长度（如 127.0.0.1）等价于 /32 或 /128。
func parseNetworks(raw, logFormat string) []netip.Prefix {
	var networks []netip.Prefix
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, ok := parseNetwork(item)
		if !ok {
			log.Printf(logFormat, item)
			continue
		}
		networks = append(networks, prefix)
	}
	return networks
}

// parseNetwork 解析单个网段，接受 CIDR 与裸地址两种写法。
func parseNetwork(item string) (netip.Prefix, bool) {
	if prefix, err := netip.ParsePrefix(item); err == nil {
		// ParsePrefix 接受带主机位的网段，这里按 Python 的 strict=False 语义掩码。
		return prefix.Masked(), true
	}
	if addr, err := netip.ParseAddr(item); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()), true
	}
	return netip.Prefix{}, false
}

// ClientIP 返回真实客户端 IP。
//
// 线上是 Cloudflare -> nginx -> 面板：nginx 用 real_ip 模块把 $remote_addr 还原成访客
// IP 并写进 X-Real-IP，这个头由 nginx 覆盖、客户端伪造不了。
//
// 但这三个头本身都是普通请求头：只要直连对端不是可信代理，客户端就能自带任意值。因此
// 先看直连对端（r.RemoteAddr 的 host 部分）是否落在可信代理网段内——不在就一律用直连
// 地址分桶，转发头完全忽略。否则攻击者轮换一个 X-Real-IP / CF-Connecting-IP 就换一个
// 限流桶，登录限流形同虚设。
func (t *Throttle) ClientIP(r *http.Request) string {
	peer := peerHost(r.RemoteAddr)
	if !t.isTrustedProxy(peer) {
		return orQuestion(peer)
	}
	for _, header := range []string{"X-Real-IP", "CF-Connecting-IP", "X-Forwarded-For"} {
		raw := r.Header.Get(header)
		if raw != "" {
			candidate := strings.TrimSpace(strings.Split(raw, ",")[0])
			if candidate != "" {
				return candidate
			}
		}
	}
	return orQuestion(peer)
}

// isTrustedProxy 判断直连对端是否落在可信代理网段内。空/非法地址按不可信处理。
func (t *Throttle) isTrustedProxy(peer string) bool {
	if peer == "" {
		return false
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	for _, network := range t.trustedProxies {
		if network.Contains(addr) {
			return true
		}
	}
	return false
}

// AdminClientAllowed 管理入口白名单：未配置 = 全部放行；回环始终放行（就绪探针与本地排障）。
func (t *Throttle) AdminClientAllowed(r *http.Request) bool {
	if len(t.adminAllow) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(t.ClientIP(r)))
	if err != nil {
		return false
	}
	// 回环地址始终放行：容器内就绪探针与本地排障不该被白名单挡住。
	if addr.IsLoopback() {
		return true
	}
	for _, network := range t.adminAllow {
		if network.Contains(addr) {
			return true
		}
	}
	return false
}

// GlobalRetryAfter 全局节流：返回需要等待的秒数，0 表示可以继续尝试登录。
func (t *Throttle) GlobalRetryAfter() int {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneGlobalLocked(now)
	if len(t.globalFails) < LoginGlobalMaxFails {
		return 0
	}
	// 与 Python 的 max(1, int(WINDOW - (now - oldest)) + 1) 对齐：int() 向零截断，
	// 这里的 Duration 整除同样向零截断；oldest 已被剪到窗口内，remain 必为正。
	remain := LoginGlobalWindow - now.Sub(t.globalFails[0])
	wait := int(remain/time.Second) + 1
	if wait < 1 {
		wait = 1
	}
	return wait
}

// Blocked 查询该 IP 是否已被单 IP 限流挡住，并返回窗口内的失败次数。
//
// 契约：本方法**只读不写失败记录**——调用方在拿到 blocked == false 时，才另行调用
// Failed(ip) 记账（Python 版是 `blocked, hits = login_blocked(ip); if not blocked:
// login_failed(ip)`）。若需要「查 + 记」一次加锁完成、避免两个并发请求都看到 5 次而各记
// 一笔（TOCTOU），用 BlockedAndRecord。
func (t *Throttle) Blocked(ip string) (bool, int) {
	key := bucketKey(ip)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	hits := t.pruneBucketLocked(key, now)
	if len(hits) > 0 {
		t.fails[key] = hits
	} else {
		// 窗口内没有失败记录：把桶删掉而不是留一个空列表。此前是 `_fails[ip] = hits`，
		// 于是每个出现过的 IP 都会永久留下一个空桶，未鉴权的登录端点可以被用来把这张表
		// 撑到任意大小。
		delete(t.fails, key)
	}
	return len(hits) >= LoginMaxFails, len(hits)
}

// Failed 记录一次登录失败。
//
// 先 trim 桶表（与 Python 的 login_failed 一致，顺序不能换：先把表压回上限，再插入新桶），
// 再把 now 追加到该 IP 的桶与全局时间线，并把全局时间线截到最近 2*LoginGlobalMaxFails 条。
func (t *Throttle) Failed(ip string) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordFailureLocked(ip, now)
}

// OK 记录一次登录成功：清空该 IP 的桶，并清空全局失败时间线。
//
// 管理员成功登录即解除全站节流：真人到场时不应被别人的失败连坐。
func (t *Throttle) OK(ip string) {
	key := bucketKey(ip)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
	t.globalFails = t.globalFails[:0]
}

// BlockedAndRecord 在一次加锁内完成「查限流 + 未挡住就记一次失败」。
//
// 语义等价于「Blocked 后紧跟 Failed」，但避免了两次加锁之间的 TOCTOU：两个并发请求
// 可能都读到 5 次失败，于是各自记一笔、实际变成 7 次才封。返回的 blocked 与 hits 与
// Blocked 的约定一致（hits 是**本次记账前**窗口内的失败次数）。
func (t *Throttle) BlockedAndRecord(ip string) (bool, int) {
	key := bucketKey(ip)
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	hits := t.pruneBucketLocked(key, now)
	if len(hits) >= LoginMaxFails {
		t.fails[key] = hits
		return true, len(hits)
	}
	t.recordFailureLocked(ip, now)
	return false, len(hits)
}

// recordFailureLocked 是 Failed / BlockedAndRecord 共用的记账体（调用方须持锁）。
func (t *Throttle) recordFailureLocked(ip string, now time.Time) {
	t.trimLocked()
	key := bucketKey(ip)
	t.fails[key] = append(t.fails[key], now)
	t.globalFails = append(t.globalFails, now)
	if len(t.globalFails) > 2*LoginGlobalMaxFails {
		// del _global_fails[:-2*LOGIN_GLOBAL_MAX_FAILS]：只留最近 40 条。
		t.globalFails = t.globalFails[len(t.globalFails)-2*LoginGlobalMaxFails:]
	}
}

// pruneGlobalLocked 剪掉全局时间线中超出窗口的条目（调用方须持锁）。
func (t *Throttle) pruneGlobalLocked(now time.Time) {
	kept := t.globalFails[:0]
	for _, ts := range t.globalFails {
		if now.Sub(ts) < LoginGlobalWindow {
			kept = append(kept, ts)
		}
	}
	t.globalFails = kept
}

// pruneBucketLocked 返回该桶在窗口内的失败时间戳（调用方须持锁）。
func (t *Throttle) pruneBucketLocked(key string, now time.Time) []time.Time {
	var kept []time.Time
	for _, ts := range t.fails[key] {
		if now.Sub(ts) < LoginWindow {
			kept = append(kept, ts)
		}
	}
	return kept
}

// trimLocked 桶数超过上限时，按「最后失败时间」丢掉最旧的一批（调用方须持锁）。
//
// 一次清掉 1/8 而不是只清 1 个：刚好卡在上限时，若每来一个请求只清一个，每个请求都要
// 排一次序；批量清掉一批能让后续请求在一段时间内直接短路返回。
func (t *Throttle) trimLocked() {
	if len(t.fails) <= LoginBucketLimit {
		return
	}
	type bucketAge struct {
		key    string
		newest time.Time
	}
	ordered := make([]bucketAge, 0, len(t.fails))
	for key, stamps := range t.fails {
		var newest time.Time // 空桶 → 零值时间，排在最前（对应 Python 的 0.0）
		for _, ts := range stamps {
			if ts.After(newest) {
				newest = ts
			}
		}
		ordered = append(ordered, bucketAge{key: key, newest: newest})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].newest.Before(ordered[j].newest)
	})
	drop := LoginBucketLimit / 8
	if drop < 1 {
		drop = 1
	}
	if drop > len(ordered) {
		drop = len(ordered)
	}
	for _, bucket := range ordered[:drop] {
		delete(t.fails, bucket.key)
	}
}

// bucketKey 是登录限流分桶键：IPv6 归并到 /64。
//
// 一个 IPv6 /64 里可以随意换地址，按 /128 分桶等于把限流交给攻击者；同一网段归一个桶
// 才能挡住轮换地址的爆破。IPv4 与无法解析的输入原样作为键。
func bucketKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip // 与 Python 的 `return str(ip)` 一致：原样返回，不做 trim
	}
	if addr.Is6() {
		// 与 str(ipaddress.ip_network("2001:db8::1/64", strict=False)) 一致 → "2001:db8::/64"。
		// 注意 Is6() 对 IPv4-mapped 地址（::ffff:1.2.3.4）也返回 true，Python 的
		// address.version 同样是 6，于是它会被归并成 "::/64"——这里刻意保持同口径。
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

// peerHost 取 r.RemoteAddr 的 host 部分（去掉端口）。
//
// Python 的 client_address 一定是 (host, port) 二元组；Go 的 RemoteAddr 通常是
// "host:port" 或 "[::1]:port"。没有端口时原样使用（测试与少数手工构造的请求会这样）。
func peerHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}

// orQuestion 空地址统一成 "?"（与 Python 的 `peer or "?"` 一致）。
func orQuestion(value string) string {
	if value == "" {
		return "?"
	}
	return value
}

// NetworksAllow 纯函数形态的网段白名单判定（面板 AdminClientAllowed 与网关 API 侧
// 中间件共用，保证两处口径一致——各写一份 CIDR 解析迟早分叉）。
//
// rawCIDRs 空 = 不限制（全部放行）；回环始终放行（就绪探针与本地排障）。
// rawCIDRs 非空但全部非法时 parseNetworks 返回空表——本函数视同「不限制」，
// 与 AdminClientAllowed 的空表分支一致：宁可放开也不把运维锁在门外
// （配置写错导致面板彻底不可达比暴露面问题更难恢复）。
func NetworksAllow(rawCIDRs, clientIP string) bool {
	networks := parseNetworks(rawCIDRs, "忽略无法解析的管理白名单网段: %q")
	if len(networks) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(clientIP))
	if err != nil {
		return false
	}
	if addr.IsLoopback() {
		return true
	}
	for _, prefix := range networks {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIPOf 提取请求的对端 IP（透传 Throttle 的可信代理口径，供 API 侧共用）。
func ClientIPOf(r *http.Request, trustedProxies string) string {
	t := &Throttle{now: time.Now}
	t.trustedProxies = parseNetworks(trustedProxies, "忽略无法解析的可信代理网段: %q")
	return t.ClientIP(r)
}
