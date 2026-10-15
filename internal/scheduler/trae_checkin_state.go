// ═══ 更新日志 ═══
// 2026-10-15：初版——Trae 签到状态机（设备代数 / 9074 退避 / 当日已签缓存）。
//
// 为什么必须落盘：这三样状态原先都只在内存（scheduler.traeDeviceGen /
// traeBackoff），进程一重启就回第 0 代、退避清零。而签到排程默认**一天只跑一次**
// （trae_check_hours=[10]），重启恰好落在 10 点前后就会让当天的重试进度全部作废。
// 参考实现（autumnsentiment/Trae2api-cn）把 device_generation / retry_backoff /
// retry_9074_count 都写进账号记录，跨重启保留，本文件对齐该口径。
//
// 存储位置：与 state.json 同目录的 trae-checkin.json，独立文件而不塞进 state.json。
// 理由：state.json 是账号池的状态机快照（冷却/熔断/EMA），由 pool 包独占读写并按
// 5s 节流落盘；签到状态是另一套生命周期（按天失效），混进去会让 pool 的
// 落盘路径多背一份与选号无关的数据，也让「今天签过没」的判定依赖 pool 的内部结构。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// traeCheckinState 一个账号的签到持久状态。
type traeCheckinState struct {
	// DeviceGeneration 设备号代数。9074 是**设备维度**的限流，换一个新派生的
	// 设备号即可绕过（实测第 0 代 9074、第 1 代立刻成功）。命中后 +1 并重试一次，
	// 之后一直沿用新代数——旧代数已确认被限流，从它重新开始只会白撞一次。
	DeviceGeneration int `json:"device_generation"`
	// RetryBackoff 上次 9074 后的退避秒数（指数退避的当前档位）。
	RetryBackoff float64 `json:"retry_backoff"`
	// Retry9074Count 连续 9074 次数（退避的指数）。
	Retry9074Count int `json:"retry_9074_count"`
	// RetryUpdatedAt 退避起算的墙钟（Unix 秒）。用墙钟而不是单调时钟，正是为了
	// 跨重启仍能算出「还要等多久」——单调时钟重启即归零，算出来的等待是负的。
	RetryUpdatedAt int64 `json:"retry_updated_at"`
	// CheckedInAt 最近一次确认「今天已签」的墙钟（Unix 秒）。
	//
	// 这是「今天签过没」的唯一判据（配合 traeBusinessDay 比较日期），不存布尔值：
	// 布尔值跨天不会自动失效，需要额外的清理任务；时间戳比较当天日期则天然失效。
	// 参考实现用同样的口径（_checkin_cache_is_today 比 checkin_status_updated_at
	// 与当前 CST 日期）。
	CheckedInAt int64 `json:"checked_in_at"`
	// Credits 最近一次领到的积分（展示用，不参与判定）。
	Credits float64 `json:"credits,omitempty"`
}

// traeCheckinStore 签到状态的落盘容器。
type traeCheckinStore struct {
	path string

	mu     sync.Mutex
	states map[string]*traeCheckinState
	dirty  bool
}

// traeCheckinFileName 状态文件名。与 state.json 同目录（由 cmd 侧传目录进来）。
const traeCheckinFileName = "trae-checkin.json"

// loadTraeCheckinStore 读取（或初始化）签到状态。文件缺失/损坏都不算错误：
// 返回空状态即可——签到状态丢了只会让当天的重试进度重来一遍，不影响正确性
// （9074 是时段性的，重试本身是安全的，见 traeCheckinState 注释）。
func loadTraeCheckinStore(dir string) *traeCheckinStore {
	store := &traeCheckinStore{states: map[string]*traeCheckinState{}}
	if dir == "" {
		return store
	}
	store.path = filepath.Join(dir, traeCheckinFileName)
	raw, err := os.ReadFile(store.path)
	if err != nil {
		return store
	}
	var payload struct {
		Accounts map[string]*traeCheckinState `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// 不隔离文件（与 pool 的 state.json 不同）：这里丢了只是重试进度归零，
		// 保留原文件供排查更有价值。
		log.Printf("WARN: [Trae签到] 状态文件解析失败，从空状态开始：%v", err)
		return store
	}
	if payload.Accounts != nil {
		store.states = payload.Accounts
	}
	return store
}

// snapshot 返回某账号状态的副本（无记录时返回零值副本，不写入 map）。
func (s *traeCheckinStore) snapshot(uid string) traeCheckinState {
	if s == nil {
		return traeCheckinState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[uid]; ok && st != nil {
		return *st
	}
	return traeCheckinState{}
}

// update 在锁内改一个账号的状态，并标记待落盘。
//
// 传函数而不是传整个结构：调用点读-改-写之间若让出锁，两个并发任务（定时入口与
// 面板手动触发）会互相覆盖对方的代数/退避。
func (s *traeCheckinStore) update(uid string, fn func(*traeCheckinState)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	st, ok := s.states[uid]
	if !ok || st == nil {
		st = &traeCheckinState{}
		s.states[uid] = st
	}
	fn(st)
	s.dirty = true
	s.mu.Unlock()
	s.flush()
}

// flush 原子落盘（临时文件 + rename）。任何失败只记日志：签到状态不是正确性依赖，
// 写不进去不该让签到任务失败。
func (s *traeCheckinStore) flush() {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	payload := struct {
		Accounts map[string]*traeCheckinState `json:"accounts"`
	}{Accounts: s.states}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	s.mu.Unlock()

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [Trae签到] 状态落盘失败：%v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("WARN: [Trae签到] 状态落盘改名失败：%v", err)
		_ = os.Remove(tmp)
	}
}

// traeBackoffDuration 按连续 9074 次数算退避时长。
//
// 基数 60s、每次翻倍、指数封顶 3 → 60/120/240/480s，之后恒定 480s。
// 为什么封顶在分钟级而不是小时级：9074 是上游「人多」这种容量信号，窗口很短
// （实测同一账号同一设备号连打 8 次全成功，说明窗口开合很快）。退避到小时级
// 只会白白错过当天可签的时机——参考实现把 TRAE_CHECKIN_9074_MAX_BACKOFF 默认
// 设成 3600s，但因为指数封顶 3，实际也到不了 480s 以上，与本实现一致。
func traeBackoffDuration(streak int) time.Duration {
	if streak < 1 {
		streak = 1
	}
	exp := streak - 1
	if exp > traeBackoffExpCap {
		exp = traeBackoffExpCap
	}
	return traeBackoffBase << exp
}

// traeBackoffRemaining 返回该账号还要等多久才该再试一次 claim。
//
// 用墙钟（RetryUpdatedAt + RetryBackoff）而不是内存里的 until，正是为了跨重启
// 仍能算出正确的等待：容器重启后若直接再打，等于把「刚被限流」的窗口又延长一次。
//
// now 由调用方传入而不是内部取 time.Now()：判定与调用方看到的是同一个时刻，
// 否则同一轮循环里「算剩余」与「判过期」会用两个不同的 now，边界上互相矛盾。
func traeBackoffRemaining(st traeCheckinState, now time.Time) time.Duration {
	if st.RetryBackoff <= 0 || st.RetryUpdatedAt <= 0 {
		return 0
	}
	deadline := time.Unix(st.RetryUpdatedAt, 0).Add(time.Duration(st.RetryBackoff * float64(time.Second)))
	if remaining := deadline.Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// traeCheckedInToday 报告该账号今天（CST 业务日）是否已确认签到。
//
// 口径与参考实现的 _checkin_cache_is_today 一致：比的是**日期**而不是 TTL，
// 所以跨天自动失效，不需要额外的清理任务。
func traeCheckedInToday(st traeCheckinState, now time.Time) bool {
	if st.CheckedInAt <= 0 {
		return false
	}
	return traeDay(time.Unix(st.CheckedInAt, 0)) == traeDay(now)
}

// traeDay 返回墙钟时刻所属的 CST 业务日（"2006-01-02"）。
//
// Trae 的签到按北京时间切天（上游是 api.trae.cn），所以判定必须用 UTC+8 而不是
// 本机时区：网关跑在容器里通常是 UTC，用本地时区会让「今天」在早上 8 点前错位
// 到昨天，导致刚签过的账号被反复重签。
func traeDay(t time.Time) string {
	return t.In(traeCheckinLocation).Format("2006-01-02")
}

// traeCheckinLocation CST 固定偏移（中国无夏令时，等价 Asia/Shanghai）。
var traeCheckinLocation = time.FixedZone("CST", 8*3600)

// traeClaimGap 两次 claim 调用之间的最小间隔（全局，跨账号）。
//
// 9074 是上游**容量信号**（"当前参与用户太多"），会随上游负载开合。连打不会
// 「造成」9074，但会把所有账号的尝试全部烧在**同一个关闭的窗口**里：实测
// 2026-10-12 01:39 四个号 5.4 秒跑完，8 次调用全 9074（含换设备号后的重试）；
// 而 01:41 把同样的调用分散在 90 秒里，三次全 success。
//
// 分散开的收益不是「更礼貌」，而是**各自撞不同的窗口**——窗口开着的那次就成了。
// 参考实现（autumnsentiment/Trae2api-cn）同样有 CHECKIN_INTERVAL 全局间隔，
// 默认 60 秒。
const (
	traeClaimGap      = 60 * time.Second
	traeBackoffBase   = 60 * time.Second
	traeBackoffExpCap = 3
	// traeRetryInterval 后台自动重试循环的扫描周期。
	//
	// 为什么需要它：签到排程默认一天只跑一次（trae_check_hours=[10]），10 点那次
	// 若撞上 9074 窗口，当天就再也没有第二次机会——线上实测正是如此
	// （2026-10-14/15 两天，16420377 与 41168546 都是 ok=4 fail=2，同一对号固定失败）。
	// 参考实现有 _checkin_auto_retry_loop（默认 60s 扫一次），本循环对齐它。
	//
	// 只扫「有过 9074 痕迹」的账号，不是通用轮询器：没有 9074 记录的账号由
	// 每日排程负责，避免把重试循环变成对上游的持续压力。
	traeRetryInterval = 60 * time.Second
)

// traeRetryPending 报告该账号是否还有未完成的重试。
//
// 判据与参考实现一致：有退避/有 9074 计数/代数 > 0 三者之一即视为「卡在 9074
// 里的账号」，且今天还没签上。DeviceGeneration 参与判据是关键——退避清零之后、
// 签上之前，账号仍应被这个循环看到（代数会一直保留）。
func traeRetryPending(st traeCheckinState, now time.Time) bool {
	if traeCheckedInToday(st, now) {
		return false
	}
	return st.RetryBackoff > 0 || st.Retry9074Count > 0 || st.DeviceGeneration > 0
}

// traeRetryDescription 生成退避的可读说明（供日志与任务详情）。
func traeRetryDescription(st traeCheckinState, now time.Time) string {
	if remaining := traeBackoffRemaining(st, now); remaining > 0 {
		return fmt.Sprintf("9074 退避中，还需 %s", remaining.Round(time.Second))
	}
	return ""
}

// traeWaitClaimGap 等到距上次 claim 满 traeClaimGap，返回是否因取消而中断。
//
// 全局节流：不是按账号，而是限制**整个进程**的发 claim 速率。理由见 traeClaimGap。
func (s *Scheduler) traeWaitClaimGap(ctx context.Context) error {
	gap := s.cfg.TraeClaimGap
	if gap <= 0 {
		gap = traeClaimGap
	}
	s.mu.Lock()
	wait := time.Until(s.traeLastClaim.Add(gap))
	s.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// traeNoteClaim 记下刚刚发出了一次 claim。
func (s *Scheduler) traeNoteClaim() {
	s.mu.Lock()
	s.traeLastClaim = time.Now()
	s.mu.Unlock()
}
