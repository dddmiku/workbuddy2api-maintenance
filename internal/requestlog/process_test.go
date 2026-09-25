// ═══ 更新日志 ═══
// 2026-09-25：使用真实子进程验证文件锁，复现热重载旧实例跨原子压缩继续追加的场景。
package requestlog

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const childMarker = "WB2A_REQUESTLOG_TEST_CHILD"

// TestRequestLogProcessHelper only acts on test-generated temporary directories;
// an ordinary test invocation never opens a production path or credential.
func TestRequestLogProcessHelper(t *testing.T) {
	if os.Getenv(childMarker) != "1" {
		t.Skip("subprocess helper")
	}
	path, prefix := os.Getenv("WB2A_REQUESTLOG_TEST_PATH"), os.Getenv("WB2A_REQUESTLOG_TEST_PREFIX")
	count, err := strconv.Atoi(os.Getenv("WB2A_REQUESTLOG_TEST_COUNT"))
	if err != nil || count < 1 || count > 100 {
		t.Fatal("invalid child count")
	}
	limit, err := strconv.Atoi(os.Getenv("WB2A_REQUESTLOG_TEST_LIMIT"))
	if err != nil {
		t.Fatal(err)
	}
	s := openFixture(t, path, Options{MaxRecords: limit})
	if os.Getenv("WB2A_REQUESTLOG_TEST_WARM") == "1" {
		appendFixture(t, s, fixtureRecord(prefix+"-before"))
		if _, err := s.List(Query{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), prefix+".ready"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(filepath.Dir(path), prefix+".start")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child start barrier timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < count; i++ {
		appendFixture(t, s, fixtureRecord(fmt.Sprintf("%s-%03d", prefix, i)))
	}
}

type childProcess struct {
	output  bytes.Buffer
	done    chan error
	cancel  context.CancelFunc
	awaited bool
}

func startChild(t *testing.T, path, prefix string, count, limit int, warm bool) *childProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	p := &childProcess{done: make(chan error, 1), cancel: cancel}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequestLogProcessHelper$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), childMarker+"=1", "WB2A_REQUESTLOG_TEST_PATH="+path, "WB2A_REQUESTLOG_TEST_PREFIX="+prefix, "WB2A_REQUESTLOG_TEST_COUNT="+strconv.Itoa(count), "WB2A_REQUESTLOG_TEST_LIMIT="+strconv.Itoa(limit))
	if warm {
		cmd.Env = append(cmd.Env, "WB2A_REQUESTLOG_TEST_WARM=1")
	}
	cmd.Stdout, cmd.Stderr = &p.output, &p.output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		if !p.awaited {
			<-p.done
			p.awaited = true
		}
	})
	return p
}

func awaitReady(t *testing.T, path, prefix string, child *childProcess) {
	t.Helper()
	ready := filepath.Join(filepath.Dir(path), prefix+".ready")
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		select {
		case err := <-child.done:
			child.awaited = true
			t.Fatalf("child exited before ready: %v\n%s", err, child.output.String())
		case <-deadline.C:
			t.Fatal("ready barrier timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func releaseChild(t *testing.T, path, prefix string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), prefix+".start"), []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func awaitChild(t *testing.T, child *childProcess) {
	t.Helper()
	err := <-child.done
	child.awaited = true
	child.cancel()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, child.output.String())
	}
}

func TestRealProcessesCommitWithoutLostRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	parent := openFixture(t, path, Options{})
	first, second := startChild(t, path, "one", 40, 0, false), startChild(t, path, "two", 40, 0, false)
	awaitReady(t, path, "one", first)
	awaitReady(t, path, "two", second)
	releaseChild(t, path, "one")
	releaseChild(t, path, "two")
	for i := 0; i < 20; i++ {
		appendFixture(t, parent, fixtureRecord(fmt.Sprintf("parent-%03d", i)))
	}
	awaitChild(t, first)
	awaitChild(t, second)
	page, err := parent.List(Query{Limit: 200})
	if err != nil || page.Total != 100 {
		t.Fatalf("cross-process records lost: %v total=%d", err, page.Total)
	}
	seen := make(map[string]bool)
	for _, item := range page.Items {
		if seen[item.RequestID] {
			t.Fatal("duplicate committed record")
		}
		seen[item.RequestID] = true
	}
	for _, id := range []string{"one-000", "one-039", "two-000", "two-039", "parent-000", "parent-019"} {
		if _, err := parent.Get(id); err != nil {
			t.Fatalf("missing %s: %v", id, err)
		}
	}
}

func TestRealStaleProcessAppendsAfterAnotherProcessCompacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	parent := openFixture(t, path, Options{MaxRecords: 6})
	stale := startChild(t, path, "old-process", 1, 6, true)
	awaitReady(t, path, "old-process", stale)
	for i := 1; i <= 8; i++ {
		appendFixture(t, parent, fixtureRecord(fmt.Sprintf("new-process-%d", i)))
	}
	releaseChild(t, path, "old-process")
	awaitChild(t, stale)
	page, err := parent.List(Query{})
	if err != nil || page.Total > 6 {
		t.Fatalf("retention failed: %v %+v", err, page)
	}
	for _, id := range []string{"new-process-8", "old-process-000"} {
		if _, err := parent.Get(id); err != nil {
			t.Fatalf("stale inode/snapshot lost %s: %v", id, err)
		}
	}
	if _, err := parent.Get("old-process-before"); err != ErrNotFound {
		t.Fatalf("stale history was resurrected: %v", err)
	}
}
