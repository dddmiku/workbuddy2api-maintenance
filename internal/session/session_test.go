package session

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/redisstore"
)

// countingStore 记录镜像调用次数的假 Store（不联网）。
type countingStore struct {
	redisstore.Noop
	mu       sync.Mutex
	setBinds int
	delBinds int
	binds    map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}
func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}
func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.binds {
		out[k] = v
	}
	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return New(Config{
		TTL:       ttl,
		Store:     store,
		Available: func() []string { return avail },
	})
}

func TestSameKeySameAccount(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")
	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	u1, _ := r.Resolve("c1")
	time.Sleep(20 * time.Millisecond)
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// 过期后可重新分配（可能巧合同号，但至少返回有效账号）。
	_ = u1
	_ = u2
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	u1, _ := r.Resolve("c1")
	if u1 != "a1" {
		t.Fatalf("initial bind=%s want a1", u1)
	}
	// a1 冷却 → 可用列表只剩 a2 → 重新分配必须换到 a2。
	r.cfg.Available = func() []string { return []string{"a2"} }
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}
	if u2 == u1 {
		t.Fatalf("bound account %s cooled but still assigned", u1)
	}
	if u2 != "a2" {
		t.Fatalf("reassigned to %s want a2", u2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串走普通 Pick；router 不会被调用）。
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		{`{"metadata":{"user_id":"mu"}}`, "mu"},                                                // metadata.user_id 兜底
		{`{"metadata":{"conversation_id":123}}`, ""},                                           // 非字符串 → 空
		{`not-json`, ""}, // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 优先于 user_id
		// Codex 客户端（codex-cli 0.155+ / 桌面端）实测形态：会话身份在 client_metadata
		// 与 prompt_cache_key 里，两者同源；没有这一层，Codex 会话完全没有粘性。
		{`{"client_metadata":{"thread_id":"t1","session_id":"s1"},"prompt_cache_key":"p1"}`, "t1"},
		{`{"client_metadata":{"session_id":"s1"},"prompt_cache_key":"p1"}`, "s1"},
		{`{"client_metadata":{"thread_id":"t1"},"metadata":{"user_id":"mu"}}`, "t1"}, // thread 优先于 user_id
		{`{"client_metadata":{"thread_id":123},"prompt_cache_key":"p1"}`, "p1"},      // 非字符串 thread → 退到 cache key
		{`{"prompt_cache_key":"p1","conversation_id":"top"}`, "top"},                 // 显式会话优先于共享 cache key
		{`{"metadata":{"user_id":"mu"},"prompt_cache_key":""}`, "mu"},                // 空 cache key 不影响 user_id 兜底
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100
	uids := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 必须拿到同一个账号（写锁 re-check 防重复分配）。
	first := ""
	for _, u := range uids {
		if u == "" {
			t.Fatal("some goroutine failed to resolve")
		}
		if first == "" {
			first = u
		}
		if u != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, u)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID 直接读绑定 uid（不触发 Resolve 的重分配），供 Bind 系列测试断言用（包内私有 helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	// Bind 幂等覆盖旧值，并异步镜像 SetBind。
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// 覆盖到 a2
	r.Bind("c1", "a2")
	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}
	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	binds := map[string]string{}
	for k, v := range st.binds {
		binds[k] = v
	}
	st.mu.Unlock()
	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}
	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}
	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // 快路径 touch → 又镜像一次
	if st.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", st.setBinds)
	}
	r.Unbind("c1")
	if st.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", st.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)
	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}
	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()
	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}
	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestOversizedSessionKeyRejected 单密钥/legacy 模式下绑定键是客户端原文，超过
// maxKeyBytes 必须拒绝：不设限时一条请求就能用接近请求体上限的 conversation_id
// 长期占住内存并镜像进 Redis（2026-09-30 深度体检发现）。
func TestOversizedSessionKeyRejected(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)

	huge := strings.Repeat("k", maxKeyBytes+1)
	if uid, ok := r.ResolveForModel(huge, "m"); ok || uid != "" {
		t.Fatalf("超长会话键必须拒绝，got uid=%q ok=%v", uid, ok)
	}
	r.Bind(huge, "a1")
	if r.Count() != 0 {
		t.Fatalf("超长会话键不得进入绑定表，count=%d", r.Count())
	}
	st.mu.Lock()
	mirrored := len(st.binds)
	st.mu.Unlock()
	if mirrored != 0 {
		t.Fatalf("超长会话键不得镜像到 Redis，mirrored=%d", mirrored)
	}

	// 上限内的长键仍正常工作（拒绝的是无界，不是长）。
	bounded := strings.Repeat("k", maxKeyBytes)
	if _, ok := r.ResolveForModel(bounded, "m"); !ok {
		t.Fatal("上限内的会话键应正常绑定")
	}
}

// TestEntryCountBounded 条目数必须有上限：原本只靠 TTL 兜底，客户端用随机会话键
// 就能把内存推成请求速率 × TTL（且每次未命中全表扫描），2026-09-30 深度体检发现。
func TestEntryCountBounded(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Hour)

	for i := 0; i < maxEntries+64; i++ {
		if _, ok := r.ResolveForModel("sess-"+strconv.Itoa(i), "m"); !ok {
			t.Fatalf("resolve sess-%d failed", i)
		}
	}
	if got := r.Count(); got > maxEntries {
		t.Fatalf("绑定条目=%d 超过上限 %d", got, maxEntries)
	}
	// 淘汰必须同步镜像删除，否则 Redis 恢复会把淘汰掉的键再带回来。
	st.mu.Lock()
	mirrored := len(st.binds)
	st.mu.Unlock()
	if mirrored > maxEntries {
		t.Fatalf("Redis 镜像条目=%d 超过上限 %d", mirrored, maxEntries)
	}
}
