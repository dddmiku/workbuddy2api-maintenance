// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中续期与重新分配都在同一写锁内完成，避免旧续期覆盖并发重绑；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
//
// ═══ 更新日志 ═══
// 2026-09-24：绑定续期、重绑和删除统一在锁内提交镜像，防止旧操作覆盖新绑定。
// 2026-09-24：增加按当前账号条件解绑，旧请求的迟到失败不再删除其他请求的新绑定。
// 2026-09-20：会话标识缺失时的对话级回退键由 handler 用 ContentKey 派生（见 ids.go）；这里保持 ExtractKey 的识别顺序不变。
// 2026-09-19：显式会话标识优先于共享缓存键，避免不同会话被缓存提示合并。
// 2026-09-18：GC 捕获本轮停止信号并等待退出，避免停止/重启后旧协程继续清理会话。
// 2026-09-30：绑定键设长度上限、条目数设上限并按最久未活跃淘汰，堵住单密钥模式下客户端
// 自选会话 ID 造成的无界内存与 Redis 镜像增长（2026-09-30 深度体检发现）。
package session

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// maxKeyBytes 会话键长度上限。多密钥模式下 ScopeKey 输出恒为 32 位 hex，但单密钥/
// legacy 模式下它原样透传客户端字符串，不设上限时单条请求就能用接近请求体上限的
// conversation_id 长期占住内存并镜像进 Redis（2026-09-30 深度体检发现）。
const maxKeyBytes = 512

// maxEntries 绑定条目数上限。条目原本只靠 TTL 兜底，稳态内存 = 唯一键数 × TTL，
// 客户端用随机 conversation_id 就能把内存推成无上限（且每次未命中都全表扫描）；
// 超限时淘汰最久未活跃的绑定，热点会话仍保持粘性（2026-09-30 深度体检发现）。
const maxEntries = 20000

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号
	// （healthy 且未占满在途，且未被该模型限流/限额）。nil 时回落 Available
	// （无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	AvailableForModel func(model string) []string
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	cfg     Config
	gcMu    sync.Mutex
	stop    chan struct{}
	gcDone  chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
func (r *Router) StartGC() {
	r.gcMu.Lock()
	defer r.gcMu.Unlock()
	if r.stop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.stop, r.gcDone = stop, done

	go func() {
		defer close(done)
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.gcMu.Lock()
	defer r.gcMu.Unlock()
	if r.stop != nil {
		close(r.stop)
		<-r.gcDone
		r.stop, r.gcDone = nil, nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		// Redis 里的键同样可能来自旧版本写下的超长 legacy 键，恢复时一并过滤。
		if _, ok := normalizeKey(key); !ok {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.evictLocked()
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的模型级冷却豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	key, ok := normalizeKey(key)
	if !ok {
		return "", false
	}
	uids := r.availableSlice(model)
	available := make(map[string]bool, len(uids))
	for _, uid := range uids {
		available[uid] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()

	if e, found := r.entries[key]; found {
		if !expired(e, now, r.cfg.TTL) && available[e.uid] {
			r.entries[key] = entry{uid: e.uid, lastActive: now}
			r.cfg.Store.SetBind(key, e.uid, r.cfg.TTL)
			return e.uid, true
		}
		delete(r.entries, key)
		r.cfg.Store.DelBind(key)
	}

	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		if !expired(v, now, r.cfg.TTL) {
			bound[v.uid] = true
		}
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	r.entries[key] = entry{uid: uid, lastActive: now}
	r.evictLocked()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	key, ok := normalizeKey(key)
	if !ok || uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = entry{uid: uid, lastActive: time.Now()}
	r.evictLocked()
	// Store 只提交异步写入；在锁内提交让镜像顺序与内存变更保持一致。
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 无条件解除会话绑定，返回是否存在。请求失败时应使用 UnbindIfUID，
// 避免旧请求清掉其他请求已更新的绑定。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
		r.cfg.Store.DelBind(key)
	}
	return found
}

// UnbindIfUID 仅在会话仍绑定到指定账号时解除绑定，返回本次是否删除。
// 校验和删除共用一把锁，失败请求携带的旧账号不能删除其他请求已换到的新账号。
func (r *Router) UnbindIfUID(key, uid string) bool {
	if key == "" || uid == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.entries[key]
	if !found || e.uid != uid {
		return false
	}
	delete(r.entries, key)
	r.cfg.Store.DelBind(key)
	return true
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSlice 安全调用可用账号函数（nil 函数视空池）。
// 优先走 AvailableForModel（带模型过滤）；未注入时回落 Available（无模型维度）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// normalizeKey 校验绑定键：空键不绑定；超过 maxKeyBytes 的键直接拒绝（不截断，
// 截断会把不同会话并到同一键上）。多密钥模式下 ScopeKey 输出定长，正常客户端不会
// 触及上限（2026-09-30 深度体检发现）。
func normalizeKey(key string) (string, bool) {
	if key == "" || len(key) > maxKeyBytes {
		return "", false
	}
	return key, true
}

// evictLocked 在绑定数超过 maxEntries 时淘汰最久未活跃的条目，直到回落到上限以内。
// 必须与插入共用写锁；淘汰时同步镜像删除，否则 Redis 恢复会把它们再带回来。
// 调用方必须已持有 r.mu。
func (r *Router) evictLocked() {
	if len(r.entries) <= maxEntries {
		return
	}
	type candidate struct {
		key        string
		lastActive time.Time
	}
	all := make([]candidate, 0, len(r.entries))
	for key, e := range r.entries {
		all = append(all, candidate{key: key, lastActive: e.lastActive})
	}
	// 最久未活跃在前；lastActive 相同时按键序，保证淘汰结果稳定可复现。
	sort.Slice(all, func(i, j int) bool {
		if !all[i].lastActive.Equal(all[j].lastActive) {
			return all[i].lastActive.Before(all[j].lastActive)
		}
		return all[i].key < all[j].key
	})
	for _, c := range all[:len(all)-maxEntries] {
		delete(r.entries, c.key)
		r.cfg.Store.DelBind(c.key)
	}
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
//
// n <= 0 直接返回 0：调用点虽然已经先判空池，但这里是取模的除数，
// 一旦将来有人把调用顺序改掉就会变成除零 panic，不值得把安全性押在调用顺序上。
func hashIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	// #nosec G115 -- n 是账号池长度，调用点已保证 > 0，且远小于 uint32 上限
	return int(h % uint32(n))
}

// ExtractKey 依次读取 metadata 会话 ID、顶层会话 ID、client_metadata 线程/会话 ID、
// prompt_cache_key、metadata.user_id。显式会话优先于缓存/用户提示。
// 调用方必须用已鉴权的调用密钥 ID 经 ScopeKey 隔离后再交给共享 Router。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	// 1. 显式会话键（OpenAI 风格 metadata.conversation_id）。
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	// 2. 顶层显式会话键不能被共享缓存键覆盖。
	for _, key := range []string{"conversation_id", "conversationId"} {
		if v := strOrEmpty(obj[key]); v != "" {
			return v
		}
	}
	// 3. Codex 客户端：client_metadata 里带 thread_id / session_id（实测 codex-cli 0.155
	//    与桌面端都会发）。漏掉这一层的话 Codex 会话完全没有粘性，同一对话会逐轮换号，
	//    上游 prompt 缓存每轮失效。
	if meta, ok := obj["client_metadata"].(map[string]any); ok {
		for _, key := range []string{"thread_id", "threadId", "session_id", "sessionId",
			"conversation_id", "conversationId"} {
			if v := strOrEmpty(meta[key]); v != "" {
				return v
			}
		}
	}
	// 4. prompt_cache_key：只在没有显式会话/线程时作为粘性提示。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 5. 最后才退到 user_id：粒度最粗（一个用户的所有对话会共用一个号）。
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
