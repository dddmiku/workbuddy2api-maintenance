// credit.go — WorkBuddy 积分查询（全部账号 + 总计），JSON 输出到 stdout。
//
// 用法:
//
//	go run ./cmd/credit        # 或编译后 ./credit
//
// 输出结构:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N,
//	          "expiring":N,"expiring_window_hours":N,"expiry":{...}},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","expiring",
//	              "expiring_window_hours","expiry":{...},"ok","error?"}]}
//
// expiry 为到期分布分档（within_1d/3d/7d/30d、later、unlimited、next_end），
// 面板的「积分有效期」页据此渲染。窗口由 WB2A_EXPIRING_SOON 控制（默认 168h）。
// 2026-10-10：新增到期分档；到期字段改用 CycleEndTime（上游不发 PackageEndTime）。
//
// realm 感知：复用 upstream.Client（auth.Parse + upstream.New），global 账号查积分
// 走 workbuddy.ai /billing/meter/*（404 回落 /v2），CN 账号维持 codebuddy.cn
// /v2/billing/meter/get-user-resource（现状逐字）。聚合口径即 upstream.ResourceSummary。
// 2026-09-16：余额工具改从凭据快照判断令牌，保持与并发安全的上游客户端契约一致。
// 2026-09-19：改为有界并发查询（默认 6 路）。原先 19 个账号串行 + 每号 200ms 间隔，
// 一次冷跑要 8.3 秒；面板 /api/state 同步等这个结果，于是"刷新网页很久才出数据"。
// 并发度用旗标/环境变量可调，默认值刻意保守：既压掉绝大部分等待，又不给上游压力。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

type accountResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain   *int64 `json:"remain"`
	Used     *int64 `json:"used"`
	Size     *int64 `json:"size"`
	Packages int    `json:"packages,omitempty"`
	// Expiring 是 Remain 中在到期窗口内即将作废的部分（Remain 的子集）。
	// 面板据此在账号行/总览卡上提示「其中 N 即将过期」。窗口由 WB2A_EXPIRING_SOON
	// 控制（默认 168h=7天，与网关 pool.expiring_soon 同口径）；查询失败或不分桶时为 0。
	Expiring int64 `json:"expiring"`
	// ExpiringWindowHours 本次分桶用的窗口小时数，供面板文案显示「N 天内」而不是
	// 硬编码 7 天（窗口可配，文案跟着走）。
	ExpiringWindowHours int `json:"expiring_window_hours,omitempty"`
	// Expiry 到期分布（固定档位，互斥，合计 = Remain）：面板「有效期」视图用。
	// 无到期时间的余额落在 unlimited（不会作废）。
	Expiry expiryView `json:"expiry"`
	OK     bool       `json:"ok"`
	Error  string     `json:"error,omitempty"`
}

// expiryView 到期分布的 JSON 形态（前端字段名用 snake_case，与面板其余字段一致）。
type expiryView struct {
	Within1d  int64  `json:"within_1d"`
	Within3d  int64  `json:"within_3d"`
	Within7d  int64  `json:"within_7d"`
	Within30d int64  `json:"within_30d"`
	Later     int64  `json:"later"`
	Unlimited int64  `json:"unlimited"`
	NextEnd   string `json:"next_end,omitempty"` // RFC3339（本地墙钟），空 = 无到期时间
	// Schedule 逐笔到期日程（升序）：某时刻会作废多少。面板据此显示具体过期时间。
	// 与分档互补——分档回答"有多少快过期"，日程回答"具体什么时候过期多少"。
	Schedule []scheduleView `json:"schedule,omitempty"`
	// ScheduleTruncated 日程是否被截断（超 maxScheduleEntries 条时余额聚成尾巴）。
	// 面板据此在末尾加一句说明，避免用户以为"就这些"。
	ScheduleTruncated bool `json:"schedule_truncated,omitempty"`
}

// scheduleView 日程单条：到期时刻 + 该时刻作废的余额。
type scheduleView struct {
	End      string `json:"end"` // RFC3339（上游 UTC+8 墙钟）
	Amount   int64  `json:"amount"`
	Packages int    `json:"packages,omitempty"`
}

func newExpiryView(b upstream.ExpiryBreakdown, schedule []upstream.ExpiryEntry, truncated bool) expiryView {
	v := expiryView{
		Within1d: b.Within1d, Within3d: b.Within3d, Within7d: b.Within7d,
		Within30d: b.Within30d, Later: b.Later, Unlimited: b.Unlimited,
		ScheduleTruncated: truncated,
	}
	if !b.NextEnd.IsZero() {
		v.NextEnd = b.NextEnd.Format(time.RFC3339)
	}
	for _, e := range schedule {
		v.Schedule = append(v.Schedule, scheduleView{
			End: e.End.Format(time.RFC3339), Amount: e.Amount, Packages: e.Packages,
		})
	}
	return v
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	up := upstream.New()
	up.GlobalEnabled = true // 允许按 realm 路由：global 账查积分走 workbuddy.ai
	accounts := collectWithConcurrency(authDir, up, concurrencyFromEnv())
	printAccounts(accounts, pretty)
}

// defaultExpiringSoon 默认到期窗口（7 天），与网关 pool.expiring_soon 默认值一致。
// 面板拿这份数据展示「其中 N 即将过期」，窗口口径必须与选号权重一致，
// 否则会出现「提示说快过期、但权重没优先消耗」的矛盾。
const defaultExpiringSoon = 168 * time.Hour

// expiringSoonFromEnv 读 WB2A_EXPIRING_SOON（Go duration 串，如 "168h"）；
// 空/非法/<=0 → 默认 7 天。
func expiringSoonFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("WB2A_EXPIRING_SOON"))
	if raw == "" {
		return defaultExpiringSoon
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultExpiringSoon
	}
	return d
}

// defaultConcurrency 默认并发度：19 个账号 6 路并发，冷跑从 8.3s 降到约 1.5s，
// 同时对上游保持温和（每路之间仍有节流）。
const defaultConcurrency = 6

// concurrencyFromEnv 读 WB2A_CREDIT_CONCURRENCY；非法或 <=0 时用默认值。
func concurrencyFromEnv() int {
	raw := os.Getenv("WB2A_CREDIT_CONCURRENCY")
	if raw == "" {
		return defaultConcurrency
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return defaultConcurrency
	}
	if value > 32 {
		return 32
	}
	return value
}

// collect 遍历 auths 目录并查询每个账号的积分摘要。供测试注入 fake upstream 断言
// realm 路由（main 从 os.Args/env 取况，collect 单一来源可测）。
// 文件清单走 auth.LoadAuthFiles（宽侧 workbuddy*.json）：与网关 LoadDir 同口径，
// 不带连字符的文件不再被跳过（P2-10，审查发现 10）。
func collect(authDir string, up *upstream.Client) []accountResult {
	return collectWithConcurrency(authDir, up, defaultConcurrency)
}

// collectWithConcurrency 并发查询各账号积分，保持与 collect 相同的输出顺序。
//
// 顺序必须稳定：面板按 auths 目录顺序展示账号，并发完成后要按原下标回填，
// 否则每次刷新账号行都会跳位置。上游 Client 自身并发安全（内部 RWMutex + 共享
// Transport 连接池），每个账号各自带自己的 token，互不干扰。
func collectWithConcurrency(authDir string, up *upstream.Client, workers int) []accountResult {
	return collectWithWindow(authDir, up, workers, expiringSoonFromEnv())
}

// collectWithWindow 是 collectWithConcurrency 的显式窗口版本（供测试注入窗口）。
func collectWithWindow(authDir string, up *upstream.Client, workers int, soon time.Duration) []accountResult {
	files, _ := auth.LoadAuthFiles(authDir)
	if workers < 1 {
		workers = 1
	}
	// slots 按文件下标回填，filled 记录哪些下标真的产出了结果：读失败/解析失败的
	// 文件与旧实现一样被静默跳过，不会在输出里留下空行。
	slots := make([]accountResult, len(files))
	filled := make([]bool, len(files))
	if len(files) == 0 {
		return []accountResult{}
	}
	if workers > len(files) {
		workers = len(files)
	}

	// 读文件 + 解析是纯本地操作，先在主协程做掉：解析失败的条目直接跳过，
	// 只有真正要发网络请求的账号才进并发池。
	type task struct {
		index int
		auth  *auth.Auth
	}
	tasks := make([]task, 0, len(files))
	for index, f := range files {
		// #nosec G304,G703 -- auth 文件路径来自命令行参数
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		res := accountResult{UID: a.UID, Nickname: a.Nickname}
		if a.Snapshot().AccessToken == "" {
			res.Error = "no accessToken"
			slots[index] = res
			filled[index] = true
			continue
		}
		tasks = append(tasks, task{index: index, auth: a})
	}

	jobs := make(chan task)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				res := accountResult{UID: item.auth.UID, Nickname: item.auth.Nickname}
				usage, err := up.ResourceUsage(item.auth, soon)
				if err != nil {
					res.Error = err.Error()
				} else {
					remain, used, size := usage.Remain, usage.Used, usage.Size
					res.Remain = &remain
					res.Used = &used
					res.Size = &size
					res.Packages = usage.Packages
					res.Expiring = usage.Expiring
					res.ExpiringWindowHours = int(soon.Hours())
					res.Expiry = newExpiryView(usage.Expiry, usage.Schedule, usage.ScheduleTruncated)
					res.OK = true
				}
				slots[item.index] = res
				filled[item.index] = true
			}
		}()
	}
	for _, item := range tasks {
		jobs <- item
	}
	close(jobs)
	wg.Wait()

	accounts := make([]accountResult, 0, len(files))
	for index := range slots {
		if filled[index] {
			accounts = append(accounts, slots[index])
		}
	}
	return accounts
}

// printAccounts 汇总并输出结果：-pretty 走人类可读日报，否则 JSON（与老版输出一致）。
func printAccounts(accounts []accountResult, pretty bool) {
	var totalRemain, totalUsed, totalSize, totalExpiring int64
	var totalExpiry expiryView
	okCount := 0
	windowHours := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
			totalExpiring += a.Expiring
			totalExpiry.Within1d += a.Expiry.Within1d
			totalExpiry.Within3d += a.Expiry.Within3d
			totalExpiry.Within7d += a.Expiry.Within7d
			totalExpiry.Within30d += a.Expiry.Within30d
			totalExpiry.Later += a.Expiry.Later
			totalExpiry.Unlimited += a.Expiry.Unlimited
			// 合计口径的最近到期：取所有账号里最早的（RFC3339 串按字典序即时间序，
			// 但为稳妥仍做真实解析比较）。
			if a.Expiry.NextEnd != "" {
				if t, err := time.Parse(time.RFC3339, a.Expiry.NextEnd); err == nil {
					if totalExpiry.NextEnd == "" {
						totalExpiry.NextEnd = a.Expiry.NextEnd
					} else if cur, err2 := time.Parse(time.RFC3339, totalExpiry.NextEnd); err2 == nil && t.Before(cur) {
						totalExpiry.NextEnd = a.Expiry.NextEnd
					}
				}
			}
			if a.ExpiringWindowHours > windowHours {
				windowHours = a.ExpiringWindowHours
			}
		}
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   totalRemain,
			"used":     totalUsed,
			"size":     totalSize,
			"accounts": len(accounts),
			"ok":       okCount,
			"failed":   len(accounts) - okCount,
			// expiring：全账号快过期积分合计（remain 的子集），面板总览卡用。
			// expiring_window_hours：本次分桶窗口，面板据此写「N 天内」文案。
			"expiring":              totalExpiring,
			"expiring_window_hours": windowHours,
			"expiry":                totalExpiry,
		},
		"accounts": accounts,
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty 人类可读日报：四行汇总，无账号明细。
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy 积分日报\n")
	fmt.Printf("账号: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("总计: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("剩余: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
