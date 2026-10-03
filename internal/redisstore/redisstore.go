// Package redisstore 封装 Upstash（Redis）持久化，并提供内存降级（Noop）。
//
// 设计约束：Upstash 走公网 TLS，单次 RTT 可能 50~300ms，因此所有写操作都是
// fire-and-forget（后台 goroutine + 失败仅 debug 日志），读操作只发生在启动时
// （加载粘性会话镜像、恢复冷却/熔断快照）。内存为主、Redis 为辅。
//
// 未配置 url / 连接失败时降级为 Noop：一切功能照常工作（纯内存模式），
// 上层只打一条启动警告日志。
// ═══ 更新日志 ═══
// 2026-09-18：关闭时排空已提交写入，并按键保持镜像写入顺序，避免旧绑定或旧状态倒序覆盖。
// 2026-09-24：正确转义连接密码并解析 REST 地址；连接串错误不再输出含凭据的原始 URL。
package redisstore

import (
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL 粘性会话镜像 + 状态快照的默认 TTL（redis 侧兜底，防脏数据长期滞留）。
const keyTTL = 7 * 24 * time.Hour

// writeConcurrencyLimit fire-and-forget 异步写的在途上限（发现 4：写 goroutine
// 无信号量限制，高写入速率下可瞬时堆积）。超过的排队不丢弃——写语义不变（见 goWrite）。
const writeConcurrencyLimit = 8

// writeQueueLimit 排队中的异步写上限（含正在执行的）。
//
// 光有 sem 只限制**并发执行数**，不限制**排队 goroutine 数**：每个提交都会起一个
// goroutine 并捕获自己的负载（SaveState 捕获的是整份池状态 JSON），Redis 慢或不可达
// 时这些 goroutine 会一直挂在 sem 上，内存与 goroutine 数随提交速率无上限增长
// ——注释承诺的「有界排队」并不成立（2026-10-02 第二轮体检发现）。
//
// 超过上限时丢弃新的写：这些是 fire-and-forget 的镜像写，本地状态才是权威来源
// （见 Store 接口注释），丢弃只影响 Redis 镜像的时效性，不影响正确性；而无限堆积
// 会拖垮进程。丢弃会打一条限频日志，避免静默。
const writeQueueLimit = 1024

var closeWaitTimeout = 10 * time.Second

// Store 只放本期需要的方法。上下文由实现内部构造（读操作配短超时，写操作 fire-and-forget）。
type Store interface {
	// SetBind 异步镜像粘性会话绑定（key→uid），带 TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 异步删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid，key 已剥前缀）；仅在启动时调用（同步）。
	// 供冷启动恢复粘性映射（防重启丢粘性）。
	LoadBinds() map[string]string
	// SaveState 异步写池状态 JSON 快照（与本地 state.json 并存，仅作恢复备份）。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用（同步）。
	LoadState() ([]byte, bool)
	// Close 关停 Store：Upstash 等待已提交的异步写全部执行完再关底层连接
	// （停机语义：最后一笔 Redis 镜像必须写完），之后新提交的写直接丢弃；幂等。
	// Noop 为空操作。进程退出前在 pool.Close() 之后调用。
	Close() error
}

const (
	bindPrefix  = "wb2api:bind:"
	stateKey    = "wb2api:state"
	readTimeout = 3 * time.Second
	// loadBindsScanTimeout 是启动时全量扫描绑定的整体上限（独立于每条 GET 的
	// readTimeout）。给得比 readTimeout 宽：启动路径可以多等一会儿，但必须有界。
	loadBindsScanTimeout = 30 * time.Second
)

// New 根据 url+token 构建 Store。
//   - url 为空 → Noop（纯内存模式）
//   - url 已是完整 rediss:// URL 则直接 ParseURL；否则用 token 组装 rediss://default:token@host:6379
//   - Ping 失败 → Noop + 启动警告（硬性降级要求：不因 Redis 不可用而失败）
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash 未配置，进入纯内存模式（Noop 降级）")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		// URL parse errors can embed the entire credential-bearing connection string.
		log.Printf("[redisstore] 警告: redis 连接串解析失败，请检查地址格式，降级 Noop")
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] 警告: upstash 连接失败 (%v)，降级 Noop（纯内存模式）", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash 已连接 (addr=%s)", opt.Addr)
	return &Upstash{
		client: client,
		sem:    make(chan struct{}, writeConcurrencyLimit),
		done:   make(chan struct{}),
	}
}

// normalizeURL 把 url+token 归一化为可直接 ParseURL 的完整 rediss:// URL。
// 完整 Redis URL 保留原有用户信息和数据库；REST 地址只取主机和端口。
// 密码使用标准 URL 转义，避免其中的 /、@、? 或 % 被当成连接串结构。
func normalizeURL(endpoint, token string) string {
	endpoint = strings.TrimSpace(endpoint)
	parsedEndpoint := endpoint
	if !strings.Contains(parsedEndpoint, "://") {
		parsedEndpoint = "//" + parsedEndpoint
	}
	parsed, err := url.Parse(parsedEndpoint)
	if err != nil {
		return endpoint // redis.ParseURL reports the invalid configuration in New.
	}
	switch parsed.Scheme {
	case "redis", "rediss":
		return endpoint
	case "", "http", "https":
	default:
		return endpoint
	}
	if parsed.Hostname() == "" {
		return endpoint
	}
	port := parsed.Port()
	if port == "" {
		port = "6379"
	}
	return (&url.URL{
		Scheme: "rediss", User: url.UserPassword("default", token),
		Host: net.JoinHostPort(parsed.Hostname(), port),
	}).String()
}

// Upstash 真实现：redis.Client 封装。
//
// 写并发上限（发现 4）：三个异步写共享 sem（cap=writeConcurrencyLimit 的信号量），
// 在途写超过上限时新写排队不丢弃——语义仍是 fire-and-forget，只是把"无限堆积"
// 收敛为"有界排队"。Close 前已提交的写（含排队中）保证执行完，Close 后新提交
// 的写直接丢弃。
type Upstash struct {
	client *redis.Client
	// sem 写信号量（有界在途写）。cap=1 时退化为串行写，供测试观察调度语义。
	sem chan struct{}
	// done 关停标志（Close 关闭）。sem 与 done 由 New 初始化；测试可直接构造
	//（client=nil，goWrite/Close 不触网络）。
	done chan struct{}
	// closeOnce 保证 Close 幂等（多次调用只关一次 done channel）。
	closeOnce sync.Once
	initOnce  sync.Once
	writeMu   sync.Mutex
	pending   sync.WaitGroup
	tails     map[string]chan struct{}
	closeErr  error
	// queued 排队中（含正在执行）的异步写数量，受 writeQueueLimit 约束。
	// 由 writeMu 保护。
	queued int
	// dropped 因队列满被丢弃的镜像写累计次数；lastDropLog 是上次打日志的时间
	// （限频用）。dropped 用原子读写，lastDropLog 由 writeMu 保护。
	dropped     atomic.Int64
	lastDropLog time.Time
}

// goWrite 以 fire-and-forget 方式执行 fn：写槽（sem）有界并发，Close 前提交的写
// 必然执行（停机镜像完整性），Close 后提交的写直接丢弃（进程已在退出）。
func (u *Upstash) goWrite(fn func()) {
	u.goWriteKey("", fn)
}

func (u *Upstash) goWriteKey(key string, fn func()) {
	u.closeOnceGuard()
	u.writeMu.Lock()
	select {
	case <-u.done:
		u.writeMu.Unlock()
		return
	default:
	}
	var previous, completed chan struct{}
	if key != "" {
		previous = u.tails[key]
		completed = make(chan struct{})
		u.tails[key] = completed
	}
	// 有界排队：超过上限就丢弃这次镜像写（见 writeQueueLimit 注释）。
	// 用 queued 计数而非 len(sem)，因为排队中的 goroutine 还没进 sem。
	if u.queued >= writeQueueLimit {
		u.writeMu.Unlock()
		u.noteDroppedWrite()
		return
	}
	u.queued++
	u.pending.Add(1)
	u.writeMu.Unlock()
	go func() {
		defer func() {
			u.writeMu.Lock()
			u.queued--
			u.writeMu.Unlock()
			u.pending.Done()
		}()
		if previous != nil {
			<-previous
		}
		u.sem <- struct{}{}
		defer func() { <-u.sem }()
		if completed != nil {
			defer func() {
				u.writeMu.Lock()
				if u.tails[key] == completed {
					delete(u.tails, key)
				}
				close(completed)
				u.writeMu.Unlock()
			}()
		}
		fn()
	}()
}

// noteDroppedWrite 记录一次因队列满而丢弃的镜像写。限频到每分钟一条，
// 避免 Redis 长时间不可用时把日志刷爆；计数始终累加，便于运维核对。
func (u *Upstash) noteDroppedWrite() {
	total := u.dropped.Add(1)
	u.writeMu.Lock()
	shouldLog := time.Since(u.lastDropLog) >= time.Minute
	if shouldLog {
		u.lastDropLog = time.Now()
	}
	u.writeMu.Unlock()
	if shouldLog {
		log.Printf("[redisstore] WARN: 异步写队列已满（上限 %d），丢弃镜像写；累计丢弃 %d 次。本地状态不受影响，Redis 镜像可能滞后",
			writeQueueLimit, total)
	}
}

// closeOnceGuard 防零值 Upstash（未经 New 构造）在 goWrite/Close 上 nil-map 式崩溃：
// sem/done 为 nil 时补建（cap=1）。仅测试会走到该路径。
func (u *Upstash) closeOnceGuard() {
	u.initOnce.Do(func() {
		if u.sem == nil {
			u.sem = make(chan struct{}, 1)
		}
		if u.done == nil {
			u.done = make(chan struct{})
		}
		u.tails = make(map[string]chan struct{})
	})
}

// Close 等待已提交的异步写全部执行完毕，再关底层 redis 连接；幂等。
// 之后新提交的写直接丢弃（goWrite 的 done 检查）。停机路径在 pool.Close() 后调用：
// pool 的最后一次 Flush→SaveState 已提交，本方法保证它写完才返回。
func (u *Upstash) Close() error {
	u.closeOnceGuard()
	u.closeOnce.Do(func() {
		u.writeMu.Lock()
		close(u.done)
		u.writeMu.Unlock()
		drained := make(chan struct{})
		go func() { u.pending.Wait(); close(drained) }()
		timer := time.NewTimer(closeWaitTimeout)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			log.Printf("[redisstore] WARN: Close 等待在途写超时，放弃（镜像可能未写完）")
			u.closeErr = errors.New("redisstore: timed out draining submitted writes")
		}
		u.closeErr = errors.Join(u.closeErr, u.closeClient())
	})
	return u.closeErr
}

// closeClient 关底层 redis 连接（client 为 nil——测试构造——时跳过）。
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}
	return u.client.Close()
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind 异步镜像粘性会话绑定。
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.goWriteKey(bindKey(key), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind 异步删除粘性会话绑定。
func (u *Upstash) DelBind(key string) {
	u.goWriteKey(bindKey(key), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState 异步写池状态 JSON 快照。
func (u *Upstash) SaveState(data []byte) {
	u.goWriteKey(stateKey, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
	})
}

// LoadState 同步读池状态快照。
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds 全量读取粘性会话绑定（SCAN bind:* 前缀）。
//
// 超时按**阶段**给，不是一个 context 管到底：此前 SCAN 与随后每个 GET 共用同一个
// 3 秒 context，重启后绑定一多（或 Redis 稍慢），后面的 GET 就集体超时，
// 结果是**静默只恢复前缀部分**绑定——没有错误、没有日志。用户看到的是重启后
// 会话粘性悄悄失效、同一会话换到别的上游账号，而该功能存在的意义正是防止这件事
// （2026-10-02 第二轮体检发现）。
//
// 现在：SCAN 有独立的整体上限；每个 GET 有自己的短超时，个别失败只丢那一条。
// 两者都保留上限，避免 Redis 不可达时这里无限阻塞。
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	scanCtx, cancelScan := context.WithTimeout(context.Background(), loadBindsScanTimeout)
	defer cancelScan()
	iter := u.client.Scan(scanCtx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(scanCtx) {
		key := iter.Val()
		// 每条 GET 独立超时：一条慢/失败不该带走整批。
		getCtx, cancelGet := context.WithTimeout(context.Background(), readTimeout)
		v, err := u.client.Get(getCtx, key).Result()
		cancelGet()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop 纯内存降级：所有方法空实现。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }
func (Noop) Close() error                          { return nil }
