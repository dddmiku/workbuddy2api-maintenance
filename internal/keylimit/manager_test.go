// ═══ 更新日志 ═══
// 2026-09-25：验证密钥隔离、精确并发上限、公平排队、撤销、跨进程额度和崩溃回收。
package keylimit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixed(p Policy) Resolver { return func() (Policy, error) { return p, nil } }

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var limited *LimitError
	if !errors.As(err, &limited) || limited.Code != code || limited.RetryAfter <= 0 {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func waitQueued(t *testing.T, m *Manager, count int, p Policy) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, err := m.Snapshot(context.Background(), map[string]Policy{"one": p})
		if err != nil {
			t.Fatal(err)
		}
		if s["one"].Queued == count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("queue did not reach expected size")
}

func TestRollingLimitAndKeyIsolation(t *testing.T) {
	m := NewMemory()
	var second atomic.Int64
	second.Store(1000)
	m.now = func() time.Time { return time.Unix(second.Load(), 0) }
	p := Policy{RequestsPerMinute: 2}
	for range 2 {
		permit, err := m.Acquire(context.Background(), "one", fixed(p))
		if err != nil {
			t.Fatal(err)
		}
		if err := permit.Release(); err != nil {
			t.Fatal(err)
		}
	}
	_, err := m.Acquire(context.Background(), "one", fixed(p))
	requireCode(t, err, "key_rate_limit")
	if _, err := m.Acquire(context.Background(), "two", fixed(p)); err != nil {
		t.Fatal("one key exhausted another key:", err)
	}
	second.Store(1060)
	_, err = m.Acquire(context.Background(), "one", fixed(p))
	requireCode(t, err, "key_rate_limit")
	second.Store(1061)
	if _, err := m.Acquire(context.Background(), "one", fixed(p)); err != nil {
		t.Fatal("expired quota did not become available:", err)
	}
}

func TestConcurrentAdmissionIsAtomicAndReleaseIdempotent(t *testing.T) {
	m := NewMemory()
	p := Policy{MaxConcurrent: 2}
	start := make(chan struct{})
	permits := make(chan *Permit, 20)
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			<-start
			permit, err := m.Acquire(context.Background(), "one", fixed(p))
			if err == nil {
				permits <- permit
			} else {
				var limited *LimitError
				if !errors.As(err, &limited) || limited.Code != "key_concurrency_limit" {
					t.Error(err)
				}
			}
		})
	}
	close(start)
	group.Wait()
	close(permits)
	if len(permits) != 2 {
		t.Fatalf("admitted %d calls instead of 2", len(permits))
	}
	for permit := range permits {
		if err := permit.Release(); err != nil {
			t.Fatal(err)
		}
		if err := permit.Release(); err != nil {
			t.Fatal(err)
		}
	}
	next, err := m.Acquire(context.Background(), "one", fixed(p))
	if err != nil {
		t.Fatal("slots were not released:", err)
	}
	_ = next.Release()
}

func TestQueueFIFOAndCancellationDoNotConsumeQuota(t *testing.T) {
	m := NewMemory()
	p := Policy{RequestsPerMinute: 10, MaxConcurrent: 1, QueueTimeoutSeconds: 2}
	first, err := m.Acquire(context.Background(), "one", fixed(p))
	if err != nil {
		t.Fatal(err)
	}
	a, b := make(chan *Permit, 1), make(chan *Permit, 1)
	go func() {
		permit, err := m.Acquire(context.Background(), "one", fixed(p))
		if err != nil {
			t.Error(err)
		}
		a <- permit
	}()
	waitQueued(t, m, 1, p)
	go func() {
		permit, err := m.Acquire(context.Background(), "one", fixed(p))
		if err != nil {
			t.Error(err)
		}
		b <- permit
	}()
	waitQueued(t, m, 2, p)
	_ = first.Release()
	pa := <-a
	if pa == nil {
		t.Fatal("first waiter was not admitted")
	}
	select {
	case <-b:
		t.Fatal("later waiter bypassed the first slot")
	default:
	}
	_ = pa.Release()
	pb := <-b
	if pb == nil {
		t.Fatal("second waiter was not admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { _, err := m.Acquire(ctx, "one", fixed(p)); canceled <- err }()
	waitQueued(t, m, 1, p)
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitQueued(t, m, 0, p)
	_ = pb.Release()
	s, err := m.Snapshot(context.Background(), map[string]Policy{"one": p})
	if err != nil || s["one"].Requests != 3 || s["one"].Active != 0 {
		t.Fatalf("waiting/canceling consumed quota or leaked slots: %+v %v", s, err)
	}
}

func TestQueueTimeoutAndLiveRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "revoked"}[revoke], func(t *testing.T) {
			m := NewMemory()
			p := Policy{MaxConcurrent: 1, QueueTimeoutSeconds: 1}
			first, _ := m.Acquire(context.Background(), "one", fixed(p))
			defer first.Release()
			var disabled atomic.Bool
			denied := errors.New("key disabled")
			result := make(chan error, 1)
			go func() {
				_, err := m.Acquire(context.Background(), "one", func() (Policy, error) {
					if disabled.Load() {
						return Policy{}, denied
					}
					return p, nil
				})
				result <- err
			}()
			waitQueued(t, m, 1, p)
			if revoke {
				disabled.Store(true)
			}
			err := <-result
			if revoke {
				if !errors.Is(err, denied) {
					t.Fatal(err)
				}
			} else {
				requireCode(t, err, "key_queue_timeout")
			}
			waitQueued(t, m, 0, p)
		})
	}
}

func TestConcurrentManagersShareQuotaAndLeases(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p := Policy{MaxConcurrent: 1, RequestsPerMinute: 2}
	one, err := a.Acquire(context.Background(), "one", fixed(p))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.Acquire(context.Background(), "one", fixed(p))
	requireCode(t, err, "key_concurrency_limit")
	_ = a.Close() // OS releases owner lock even if a dying request cannot release.
	two, err := b.Acquire(context.Background(), "one", fixed(p))
	if err != nil {
		t.Fatal("dead owner slot was not reclaimed:", err)
	}
	_ = two.Release()
	_ = one.Release()
	_, err = b.Acquire(context.Background(), "one", fixed(p))
	requireCode(t, err, "key_rate_limit")
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if bytes.Contains(raw, []byte(`"one"`)) {
		t.Fatal("raw key identity persisted")
	}
}

func TestCrashHelper(t *testing.T) {
	dir := os.Getenv("WB2API_KEY_LIMIT_CRASH_FIXTURE")
	if dir == "" {
		t.Skip("child process entry point")
	}
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Acquire(context.Background(), "one", fixed(Policy{RequestsPerMinute: 2, MaxConcurrent: 1})); err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // Deliberately skip manager/permit cleanup to exercise OS lock recovery.
}

func TestProcessCrashReclaimsSlotWithoutResettingQuota(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelper$")
	cmd.Env = append(os.Environ(), "WB2API_KEY_LIMIT_CRASH_FIXTURE="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	p := Policy{RequestsPerMinute: 2, MaxConcurrent: 1}
	got, err := m.Acquire(context.Background(), "one", fixed(p))
	if err != nil {
		t.Fatal(err)
	}
	_ = got.Release()
	_, err = m.Acquire(context.Background(), "one", fixed(p))
	requireCode(t, err, "key_rate_limit")
}

func TestCorruptStateIsNotSilentlyReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	raw := []byte(`{"version":99,"keys":{}}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if manager, err := Open(dir); err == nil {
		manager.Close()
		t.Fatal("invalid state accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("invalid state was overwritten")
	}
}

func TestContendedStateLockHonorsContext(t *testing.T) {
	m, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	f, ok, err := tryFileLock(filepath.Join(m.dir, "state.lock"))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer unlockFile(f)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = m.Acquire(ctx, "one", fixed(Policy{MaxConcurrent: 1}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
