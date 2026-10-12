// ═══ 更新日志 ═══
// 2026-10-13：初版——积分查询的共享缓存（与 panel/app.py 的 get_credits 同口径）。
//
// 为什么需要这一层而不是「每次请求都跑一次 ./credit」：./credit 要遍历全部账号打
// 上游接口，冷跑 8 秒起步。面板首屏、账号列表、有效期页都会读积分，各跑一次既慢
// 又会被上游当异常流量。
//
// 三条不变量（都是踩过坑才有的）：
//
//  1. **普通读取不等待**。首屏拿到的是缓存快照 + pending 标记，前端自己稍后重取，
//     而不是让整个页面卡在一次冷查询上。
//  2. **失败不覆盖成功数据**。查询失败时保留上一次的余额，只附上错误文案并安排
//     延迟重试；否则一次网络抖动就会让面板显示「积分 0」。
//  3. **并发查询合并成一次**。普通读取与手动刷新共用同一个在途查询及其结果，
//     后到的调用者不会另起一次查询、也不会读到别人的中间态。
package panel

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 默认的缓存节奏，与 panel/app.py 的 CREDIT_TTL / CREDIT_RETRY_INTERVAL 一致。
const (
	CreditTTL           = 60 * time.Second
	CreditRetryInterval = 30 * time.Second
)

// creditRefresh 是一次在途查询；done 关闭即表示结果已写入 result。
type creditRefresh struct {
	done   chan struct{}
	result map[string]any
}

// CreditRunner 执行 ./credit 辅助程序并返回它的原始 JSON 输出。
//
// 抽象成接口是为了两件事：容器版经 docker exec、原生版直接起子进程；以及测试里
// 不必真的跑一个 8 秒的查询。
type CreditRunner interface {
	RunCredit(ctx context.Context, env map[string]string) ([]byte, error)
}

// CreditCache 协调积分查询的共享与缓存。
type CreditCache struct {
	mu     sync.Mutex
	runner CreditRunner
	// configPath 用于读 pool.expiring_soon，把「N 天内过期」的口径与网关对齐。
	configPath string
	ttl        time.Duration
	retry      time.Duration
	now        func() time.Time

	ts      time.Time
	data    map[string]any
	errMsg  string
	retryAt time.Time

	refreshing *creditRefresh
}

// NewCreditCache 建一个缓存；ttl/retry 传 0 时用默认值。
func NewCreditCache(runner CreditRunner, configPath string, ttl, retry time.Duration) *CreditCache {
	if ttl <= 0 {
		ttl = CreditTTL
	}
	if retry <= 0 {
		retry = CreditRetryInterval
	}
	return &CreditCache{runner: runner, configPath: configPath, ttl: ttl, retry: retry, now: time.Now}
}

// Get 取积分快照；force=true 时等待一次新查询完成。
func (c *CreditCache) Get(ctx context.Context, force bool) map[string]any {
	c.mu.Lock()
	now := c.now()
	var due bool
	if c.errMsg != "" {
		// 上次失败：按重试间隔安排下一次，而不是每个请求都去打上游。
		due = !now.Before(c.retryAt)
	} else {
		due = c.data == nil || now.Sub(c.ts) >= c.ttl
	}
	var refresh *creditRefresh
	if force || due {
		refresh = c.startLocked(ctx)
	} else {
		refresh = c.refreshing
	}
	if !force {
		// 普通读取立即返回：等待会让首屏卡在一次冷查询上。
		snapshot := c.snapshotLocked()
		c.mu.Unlock()
		return snapshot
	}
	c.mu.Unlock()

	if refresh != nil {
		select {
		case <-refresh.done:
		case <-ctx.Done():
			// 调用方超时：返回当前快照（含 pending），不要卡住 HTTP 处理协程。
			c.mu.Lock()
			snapshot := c.snapshotLocked()
			c.mu.Unlock()
			return snapshot
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return copyJSONMap(refresh.result)
}

// snapshotLocked 组装给前端的快照（调用方须持锁）。
func (c *CreditCache) snapshotLocked() map[string]any {
	result := copyJSONMap(c.data)
	if result == nil {
		result = map[string]any{"accounts": []any{}}
	}
	result["pending"] = c.refreshing != nil
	if c.errMsg != "" {
		result["error"] = c.errMsg
	}
	return result
}

// startLocked 起一次后台查询；已有在途查询时复用它（调用方须持锁）。
func (c *CreditCache) startLocked(ctx context.Context) *creditRefresh {
	if c.refreshing != nil {
		return c.refreshing
	}
	refresh := &creditRefresh{done: make(chan struct{}), result: nil}
	c.refreshing = refresh
	// 查询用独立的 context：它由缓存协调，不随某一次请求的取消而中断——
	// 否则「用户刷新页面」会把这次查询掐掉，后到的请求又得从头开始。
	queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	go func() {
		defer cancel()
		data := c.query(queryCtx)
		c.mu.Lock()
		c.finishLocked(refresh, data)
		c.mu.Unlock()
	}()
	return refresh
}

// finishLocked 提交查询结果（调用方须持锁）。
func (c *CreditCache) finishLocked(refresh *creditRefresh, data map[string]any) {
	if c.refreshing == refresh {
		now := c.now()
		if message := jsonString(data["error"]); message != "" {
			// 失败：保留旧数据，只记错误与重试时刻。
			c.errMsg = message
			c.retryAt = now.Add(c.retry)
		} else {
			c.ts, c.data, c.errMsg, c.retryAt = now, data, "", time.Time{}
		}
		c.refreshing = nil
	}
	refresh.result = c.snapshotLocked()
	refresh.result["pending"] = false
	close(refresh.done)
}

// query 执行一次积分查询并把结果规整成 {accounts:[...], total:…} 形态。
//
// 出错时返回带 error 键的对象而不是 Go error：调用方（缓存）只需要「有没有错、
// 错误文案是什么」，包成 error 反而要在两处再拆一遍。
func (c *CreditCache) query(ctx context.Context) map[string]any {
	if c.runner == nil {
		return map[string]any{"error": "积分查询不可用"}
	}
	env := map[string]string{}
	if soon := c.expiringSoonEnv(); soon != "" {
		env["WB2A_EXPIRING_SOON"] = soon
	}
	raw, err := c.runner.RunCredit(ctx, env)
	if err != nil {
		return map[string]any{"error": strings.TrimSpace(err.Error())}
	}
	value, err := parseOrderedJSON(raw)
	if err != nil {
		return map[string]any{"error": "credit 输出无法解析", "raw": truncateText(string(raw), 400)}
	}
	object, ok := value.(*orderedObject)
	if !ok {
		return map[string]any{"error": "credit 输出格式无效"}
	}
	decoded := orderedToNative(object)
	accounts, ok := decoded["accounts"].([]any)
	if !ok {
		return map[string]any{"error": "credit 输出格式无效"}
	}
	for _, account := range accounts {
		if _, ok := account.(map[string]any); !ok {
			return map[string]any{"error": "credit 输出格式无效"}
		}
	}
	return decoded
}

// durationShape 只接受 Go duration 形状（数字 + 单位），避免把任意配置值塞进 env。
var durationShape = regexp.MustCompile(`^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$`)

// expiringSoonEnv 把网关 pool.expiring_soon 透传给 ./credit。
//
// 读不到/非法 → 返回空（credit 自己回落默认 7 天）。配置是管理员可控的
// （pool.expiring_soon），面板文案必须跟着它走，否则会出现「面板说 3 天内要过期、
// 实际网关按 7 天在加权」的错位。
func (c *CreditCache) expiringSoonEnv() string {
	if c.configPath == "" {
		return ""
	}
	value := configNestedString(c.configPath, "pool", "expiring_soon")
	if durationShape.MatchString(value) {
		return value
	}
	return ""
}

// CreditsByUID 把查询结果按 uid 建索引，返回 (索引, 原始结果)。
func CreditsByUID(data map[string]any) (map[string]map[string]any, map[string]any) {
	index := map[string]map[string]any{}
	accounts, _ := data["accounts"].([]any)
	for _, item := range accounts {
		account, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if uid := jsonString(account["uid"]); uid != "" {
			index[uid] = account
		}
	}
	return index, data
}

// copyJSONMap 浅拷贝一个 JSON 对象（顶层），nil 输入返回 nil。
func copyJSONMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

// truncateText 截断到 n 字节（按 UTF-8 边界回退），用于把原始输出放进错误提示。
func truncateText(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	for n > 0 && !isRuneStart(text[n]) {
		n--
	}
	return text[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// orderedToNative 把保序对象转回普通 map/slice，便于统一按 map[string]any 读取。
//
// 只在解析外部输出（./credit 的 JSON）时用：那些数据没有保序需求，转成原生形态
// 才能和网关返回的 JSON 走同一套取值 helper。
func orderedToNative(value any) map[string]any {
	object, ok := value.(*orderedObject)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(object.keys))
	for _, key := range object.keys {
		out[key] = nativeValue(object.vals[key])
	}
	return out
}

func nativeValue(value any) any {
	switch v := value.(type) {
	case *orderedObject:
		return orderedToNative(v)
	case []any:
		items := make([]any, 0, len(v))
		for _, item := range v {
			items = append(items, nativeValue(item))
		}
		return items
	default:
		return value
	}
}
