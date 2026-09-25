// ═══ 更新日志 ═══
// 2026-09-25：共享滑动窗口和并发租约，热重载不重置额度，崩溃回收且排队有界。
package keylimit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxStateBytes = 4 << 20
const maxStateKeys = 512

type bucket struct {
	Second int64 `json:"second"`
	Count  int   `json:"count"`
}

type ticket struct {
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Deadline int64  `json:"deadline"`
}

type keyState struct {
	Buckets []bucket          `json:"buckets,omitempty"`
	Running map[string]string `json:"running,omitempty"`
	Waiting []ticket          `json:"waiting,omitempty"`
}

type document struct {
	Version int                  `json:"version"`
	Keys    map[string]*keyState `json:"keys"`
}

type Manager struct {
	mu        sync.Mutex
	dir       string
	owner     string
	ownerFile *os.File
	memory    document
	completed map[string]bool
	closed    bool
	now       func() time.Time
	wake      chan struct{}
}

type Permit struct {
	Wait     time.Duration
	Snapshot Snapshot
	manager  *Manager
	key, id  string
	once     sync.Once
	err      error
}

// NewMemory shares the exact admission algorithm but has no cross-process
// state. Production uses Open; embedded test handlers can use NewMemory.
func NewMemory() *Manager {
	return &Manager{owner: rand.Text(), memory: document{Version: 1, Keys: map[string]*keyState{}},
		completed: map[string]bool{}, now: time.Now, wake: make(chan struct{}, 1)}
}

func Open(dir string) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("key limit state directory is empty")
	}
	if err := os.MkdirAll(filepath.Join(dir, "owners"), 0700); err != nil {
		return nil, err
	}
	m := NewMemory()
	m.dir = dir
	f, acquired, err := tryFileLock(m.ownerPath(m.owner))
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, errors.New("key limit owner identity collision")
	}
	m.ownerFile = f
	// This directory is private application state; reclaim only known nonce
	// files whose process locks have been released.
	names, err := os.ReadDir(filepath.Join(dir, "owners"))
	if err == nil {
		for _, entry := range names {
			owner := strings.TrimSuffix(entry.Name(), ".lock")
			if owner != m.owner && entry.Name() == owner+".lock" && validOwner(owner) {
				_, _ = m.ownerAlive(owner)
			}
		}
	}
	if err = m.transact(context.Background(), func(*document) error { return nil }); err != nil {
		_ = m.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) ownerPath(owner string) string {
	return filepath.Join(m.dir, "owners", owner+".lock")
}

func validOwner(owner string) bool {
	if len(owner) < 20 || len(owner) > 64 {
		return false
	}
	for _, ch := range owner {
		if !((ch >= 'A' && ch <= 'Z') || (ch >= '2' && ch <= '7')) {
			return false
		}
	}
	return true
}

func stateKey(id string) string {
	hash := sha256.Sum256([]byte(id))
	return hex.EncodeToString(hash[:])
}

func (m *Manager) ownerAlive(owner string) (bool, error) {
	if owner == m.owner {
		return !m.closed, nil
	}
	if !validOwner(owner) {
		return false, errors.New("invalid key limit owner")
	}
	if m.dir == "" {
		return false, nil
	}
	path := m.ownerPath(owner)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("invalid key limit owner file")
	}
	f, acquired, err := tryFileLock(path)
	if err != nil || !acquired {
		return !acquired && err == nil, err
	}
	unlockFile(f)
	_ = os.Remove(path)
	return false, nil
}

func (m *Manager) prune(doc *document, now time.Time) error {
	alive := map[string]bool{}
	checkOwner := func(owner string) (bool, error) {
		if value, exists := alive[owner]; exists {
			return value, nil
		}
		value, err := m.ownerAlive(owner)
		if err == nil {
			alive[owner] = value
		}
		return value, err
	}
	for key, state := range doc.Keys {
		rawKey, err := hex.DecodeString(key)
		if err != nil || len(rawKey) != sha256.Size || state == nil || len(state.Buckets) > 62 || len(state.Running) > MaxConcurrent || len(state.Waiting) > MaxQueuedPerKey {
			return errors.New("invalid key limit state")
		}
		buckets := state.Buckets[:0]
		for index, item := range state.Buckets {
			if item.Count <= 0 || item.Count > MaxRequestsPerMinute || item.Second < 0 ||
				(index > 0 && item.Second <= state.Buckets[index-1].Second) {
				return errors.New("invalid key limit bucket")
			}
			if item.Second+61 > now.Unix() {
				buckets = append(buckets, item)
			}
		}
		state.Buckets = buckets
		for id, owner := range state.Running {
			live, err := checkOwner(owner)
			if err != nil || !validOwner(id) {
				return errors.New("invalid key limit lease")
			}
			if !live || m.completed[id] {
				delete(state.Running, id)
			}
		}
		waiting := state.Waiting[:0]
		for _, item := range state.Waiting {
			live, err := checkOwner(item.Owner)
			if err != nil || !validOwner(item.ID) {
				return errors.New("invalid key limit waiter")
			}
			if live && item.Deadline > now.UnixMilli() && !m.completed[item.ID] {
				waiting = append(waiting, item)
			}
		}
		state.Waiting = waiting
		if len(state.Buckets)+len(state.Running)+len(state.Waiting) == 0 {
			delete(doc.Keys, key)
		}
	}
	return nil
}

func (m *Manager) transact(ctx context.Context, change func(*document) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for !m.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	doc := &m.memory
	if m.dir != "" {
		var guard *os.File
		for {
			f, acquired, err := tryFileLock(filepath.Join(m.dir, "state.lock"))
			if err != nil {
				return err
			}
			if acquired {
				guard = f
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		defer unlockFile(guard)
		loaded, err := m.read()
		if err != nil {
			return err
		}
		doc = &loaded
	}
	if err := m.prune(doc, m.now()); err != nil {
		return err
	}
	result := change(doc)
	if m.dir != "" {
		if err := m.write(doc); err != nil {
			return err
		}
	}
	clear(m.completed)
	return result
}

func (m *Manager) read() (document, error) {
	path := filepath.Join(m.dir, "state.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return document{Version: 1, Keys: map[string]*keyState{}}, nil
	}
	if err != nil {
		return document{}, err
	}
	if !info.Mode().IsRegular() {
		return document{}, errors.New("key limit state is not a regular file")
	}
	// #nosec G304 -- fixed filename under the administrator-owned state directory.
	f, err := os.Open(path)
	if err != nil {
		return document{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil || len(data) > maxStateBytes {
		return document{}, errors.New("key limit state exceeds its read bound")
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil || doc.Version != 1 || doc.Keys == nil || len(doc.Keys) > maxStateKeys {
		return document{}, errors.New("key limit state is invalid")
	}
	return doc, nil
}

func (m *Manager) write(doc *document) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes || len(doc.Keys) > maxStateKeys {
		return ErrCapacity
	}
	f, err := os.CreateTemp(m.dir, ".limits-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, filepath.Join(m.dir, "state.json"))
}

func snapshot(state *keyState, policy Policy, now time.Time) Snapshot {
	value := Snapshot{Limited: !policy.Unlimited(), Policy: policy}
	if state == nil {
		value.Remaining = policy.RequestsPerMinute
		return value
	}
	value.Active, value.Queued = len(state.Running), len(state.Waiting)
	for _, item := range state.Buckets {
		value.Requests += item.Count
	}
	value.Remaining = max(0, policy.RequestsPerMinute-value.Requests)
	if policy.RequestsPerMinute > 0 && value.Requests >= policy.RequestsPerMinute && len(state.Buckets) > 0 {
		value.RetryAfterSeconds = max(1, int(state.Buckets[0].Second+61-now.Unix()))
	}
	return value
}

func removeWaiter(state *keyState, id string) {
	for index, item := range state.Waiting {
		if item.ID == id {
			state.Waiting = append(state.Waiting[:index], state.Waiting[index+1:]...)
			return
		}
	}
}

func (m *Manager) Acquire(ctx context.Context, keyID string, resolve Resolver) (*Permit, error) {
	if keyID == "" || resolve == nil {
		return nil, errors.New("key limit requires an identity and policy")
	}
	start := m.now()
	key, id := stateKey(keyID), rand.Text()
	var deadline time.Time
	queued, admitted := false, false
	defer func() {
		if queued && !admitted {
			_ = m.release(key, id)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		policy, err := resolve()
		if err != nil {
			return nil, err
		}
		if err := policy.Validate(); err != nil {
			return nil, err
		}
		if policy.Unlimited() {
			return &Permit{Wait: m.now().Sub(start), Snapshot: Snapshot{Policy: policy}}, nil
		}
		var info Snapshot
		wait := false
		err = m.transact(ctx, func(doc *document) error {
			now := m.now()
			state := doc.Keys[key]
			if state == nil {
				if len(doc.Keys) >= maxStateKeys {
					return ErrCapacity
				}
				state = &keyState{Running: map[string]string{}}
				doc.Keys[key] = state
			}
			if state.Running == nil {
				state.Running = map[string]string{}
			}
			info = snapshot(state, policy, now)
			if queued && !now.Before(deadline) {
				removeWaiter(state, id)
				return &LimitError{Code: "key_queue_timeout", RetryAfter: time.Second, Snapshot: info}
			}
			if policy.RequestsPerMinute > 0 && info.Requests >= policy.RequestsPerMinute {
				removeWaiter(state, id)
				return &LimitError{Code: "key_rate_limit", RetryAfter: time.Duration(info.RetryAfterSeconds) * time.Second, Snapshot: info}
			}
			head := policy.MaxConcurrent == 0 || len(state.Waiting) == 0 || state.Waiting[0].ID == id
			full := policy.MaxConcurrent > 0 && len(state.Running) >= policy.MaxConcurrent
			if full || !head {
				if policy.QueueTimeoutSeconds == 0 {
					removeWaiter(state, id)
					return &LimitError{Code: "key_concurrency_limit", RetryAfter: time.Second, Snapshot: info}
				}
				bound := start.Add(time.Duration(policy.QueueTimeoutSeconds) * time.Second)
				if deadline.IsZero() || bound.Before(deadline) {
					deadline = bound
				}
				if !now.Before(deadline) {
					removeWaiter(state, id)
					return &LimitError{Code: "key_queue_timeout", RetryAfter: time.Second, Snapshot: info}
				}
				found := false
				for index := range state.Waiting {
					if state.Waiting[index].ID == id {
						state.Waiting[index].Deadline = deadline.UnixMilli()
						found = true
					}
				}
				if !found {
					total := 0
					for _, row := range doc.Keys {
						total += len(row.Waiting)
					}
					if len(state.Waiting) >= MaxQueuedPerKey || total >= MaxQueuedTotal {
						return &LimitError{Code: "key_queue_full", RetryAfter: time.Second, Snapshot: info}
					}
					state.Waiting = append(state.Waiting, ticket{ID: id, Owner: m.owner, Deadline: deadline.UnixMilli()})
				}
				queued, wait = true, true
				return nil
			}
			removeWaiter(state, id)
			if policy.MaxConcurrent > 0 {
				total := 0
				for _, row := range doc.Keys {
					total += len(row.Running)
				}
				if total >= MaxRunningTotal {
					return ErrCapacity
				}
				state.Running[id] = m.owner
			}
			if policy.RequestsPerMinute > 0 {
				second := now.Unix()
				n := len(state.Buckets)
				if n > 0 && state.Buckets[n-1].Second >= second {
					state.Buckets[n-1].Count++
				} else {
					state.Buckets = append(state.Buckets, bucket{Second: second, Count: 1})
				}
			}
			info = snapshot(state, policy, now)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !wait {
			admitted = true
			permit := &Permit{Wait: m.now().Sub(start), Snapshot: info}
			if policy.MaxConcurrent > 0 {
				permit.manager, permit.key, permit.id = m, key, id
			}
			return permit, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.wake:
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *Manager) release(key, id string) error {
	m.mu.Lock()
	m.completed[id] = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.transact(ctx, func(doc *document) error {
		if state := doc.Keys[key]; state != nil {
			delete(state.Running, id)
			removeWaiter(state, id)
		}
		return nil
	})
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return err
}

func (p *Permit) Release() error {
	if p == nil || p.manager == nil {
		return nil
	}
	p.once.Do(func() { p.err = p.manager.release(p.key, p.id) })
	return p.err
}

func (m *Manager) Snapshot(ctx context.Context, policies map[string]Policy) (map[string]Snapshot, error) {
	result := make(map[string]Snapshot, len(policies))
	err := m.transact(ctx, func(doc *document) error {
		for key, policy := range policies {
			if err := policy.Validate(); err != nil {
				return err
			}
			result[key] = snapshot(doc.Keys[stateKey(key)], policy, m.now())
		}
		return nil
	})
	return result, err
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	unlockFile(m.ownerFile)
	if m.dir != "" {
		err := os.Remove(m.ownerPath(m.owner))
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
