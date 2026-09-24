// ═══ 更新日志 ═══
// 2026-09-24：复现 Windows 临时只读句柄占用造成账本原子替换失败，验证短暂占用后能落盘。
//go:build windows

package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFlushSurvivesTransientReadHandleOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	store, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	store.Record("key", "name", "mask", "model", 10, 2, 0, 0, false, time.Now())
	finished := make(chan error, 1)
	go func() { finished <- store.Flush() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("flush gave up during a transient reader lock")
		}
	case <-time.After(30 * time.Millisecond):
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-finished; err != nil {
			t.Fatalf("flush did not recover after reader closed: %v", err)
		}
	}
	if got := readIntegrityDocument(t, path).Totals.TotalTokens; got != 12 {
		t.Fatalf("persisted usage = %d, want 12", got)
	}
}
