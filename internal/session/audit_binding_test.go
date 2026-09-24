// ═══ 更新日志 ═══
// 2026-09-24：复现旧绑定/删除在锁外提交 Redis 镜像时倒序覆盖新会话绑定。
// 2026-09-24：复现旧请求的迟到失败删除已由其他请求重绑的新账号。
package session

import (
	"sync"
	"testing"
	"time"
)

type auditBlockingBindStore struct {
	*countingStore
	blockKind string
	blockUID  string
	once      sync.Once
	entered   chan struct{}
	proceed   chan struct{}
}

func (s *auditBlockingBindStore) block() {
	s.once.Do(func() {
		close(s.entered)
		<-s.proceed
	})
}

func (s *auditBlockingBindStore) SetBind(key, uid string, ttl time.Duration) {
	if s.blockKind == "set" && uid == s.blockUID {
		s.block()
	}
	s.countingStore.SetBind(key, uid, ttl)
}

func (s *auditBlockingBindStore) DelBind(key string) {
	if s.blockKind == "delete" {
		s.block()
	}
	s.countingStore.DelBind(key)
}

func TestAuditMirrorMutationOrderMatchesBindingOrder(t *testing.T) {
	for _, operation := range []string{"bind", "resolve", "unbind", "conditional_unbind", "gc"} {
		t.Run(operation, func(t *testing.T) {
			store := &auditBlockingBindStore{
				countingStore: newCountingStore(),
				entered:       make(chan struct{}),
				proceed:       make(chan struct{}),
			}
			r := routerWith(store, []string{"old", "first", "new"}, time.Minute)
			r.Bind("conversation", "old")
			store.blockKind = "delete"
			var first func()
			switch operation {
			case "bind":
				store.blockKind, store.blockUID = "set", "first"
				first = func() { r.Bind("conversation", "first") }
			case "resolve":
				store.blockKind, store.blockUID = "set", "old"
				first = func() { r.ResolveForModel("conversation", "model") }
			case "unbind":
				first = func() { r.Unbind("conversation") }
			case "conditional_unbind":
				first = func() { r.UnbindIfUID("conversation", "old") }
			case "gc":
				r.mu.Lock()
				r.entries["conversation"] = entry{uid: "old", lastActive: time.Now().Add(-time.Hour)}
				r.mu.Unlock()
				first = func() { r.gcOnce(time.Now()) }
			}
			var release sync.Once
			unblock := func() { release.Do(func() { close(store.proceed) }) }
			defer unblock()
			firstDone := make(chan struct{})
			go func() { first(); close(firstDone) }()
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("first mutation did not reach the persistence boundary")
			}
			secondDone := make(chan struct{})
			go func() { r.Bind("conversation", "new"); close(secondDone) }()
			// 停住旧写入，让新绑定有机会先完成；正确实现会保持它们的提交顺序。
			select {
			case <-secondDone:
			case <-time.After(30 * time.Millisecond):
			}
			unblock()
			for _, done := range []chan struct{}{firstDone, secondDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("binding mutation did not finish")
				}
			}
			r.mu.RLock()
			local := r.entries["conversation"].uid
			r.mu.RUnlock()
			persisted := store.LoadBinds()["conversation"]
			if local != "new" || persisted != local {
				t.Fatalf("new binding overwritten by delayed %s mirror: local=%q persisted=%q", operation, local, persisted)
			}
		})
	}
}

func TestAuditDelayedFailureKeepsNewAccountBinding(t *testing.T) {
	store := newCountingStore()
	r := routerWith(store, []string{"old", "new"}, time.Minute)
	r.Bind("conversation", "old")
	failedUID, ok := r.ResolveForModel("conversation", "model")
	if !ok || failedUID != "old" {
		t.Fatal("old request did not capture the original sticky account")
	}
	failed := make(chan struct{})
	removed := make(chan bool, 1)
	go func() {
		<-failed
		removed <- r.UnbindIfUID("conversation", failedUID)
	}()
	// 新请求在旧请求失败前已经成功换号，与 handler 成功路径的 Bind 一致。
	r.Bind("conversation", "new")
	close(failed)
	if <-removed {
		t.Error("late failure from old account removed another request's new binding")
	}
	if uid, ok := r.boundUID("conversation"); !ok || uid != "new" {
		t.Errorf("new local binding lost after old request failed: uid=%q found=%t", uid, ok)
	}
	if uid := store.LoadBinds()["conversation"]; uid != "new" {
		t.Errorf("new persisted binding lost after old request failed: uid=%q", uid)
	}
}

func TestAuditConditionalUnbindOnlyDeletesMatchingAccount(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		uid        string
		wantRemove bool
	}{
		{name: "matches", key: "conversation", uid: "account", wantRemove: true},
		{name: "different_account", key: "conversation", uid: "old-account"},
		{name: "different_session", key: "other", uid: "account"},
		{name: "missing_account", key: "conversation"},
		{name: "missing_session", uid: "account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newCountingStore()
			r := routerWith(store, []string{"account"}, time.Minute)
			r.Bind("conversation", "account")
			r.Bind("unrelated", "account")
			if got := r.UnbindIfUID(tc.key, tc.uid); got != tc.wantRemove {
				t.Errorf("UnbindIfUID removed=%t want %t", got, tc.wantRemove)
			}
			if r.UnbindIfUID(tc.key, tc.uid) {
				t.Error("conditional unbind must be idempotent")
			}
			_, found := r.boundUID("conversation")
			if found == tc.wantRemove {
				t.Errorf("unexpected local binding presence: found=%t", found)
			}
			_, mirrored := store.LoadBinds()["conversation"]
			if mirrored != found {
				t.Error("conditional unbind did not preserve local/mirror agreement")
			}
			if uid, ok := r.boundUID("unrelated"); !ok || uid != "account" {
				t.Error("conditional unbind changed a different session on the same account")
			}
			store.mu.Lock()
			deletes := store.delBinds
			store.mu.Unlock()
			wantDeletes := 0
			if tc.wantRemove {
				wantDeletes = 1
			}
			if deletes != wantDeletes {
				t.Errorf("mirror deletes=%d want %d", deletes, wantDeletes)
			}
		})
	}
}
