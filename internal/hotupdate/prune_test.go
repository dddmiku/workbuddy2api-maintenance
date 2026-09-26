// ═══ 更新日志 ═══
// 2026-09-26：锁定更新目录清理：保留当前与上一个运行目录，不动指针与非本工具文件。
package hotupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneUpdateDirKeepsCurrentAndPrevious(t *testing.T) {
	dir := t.TempDir()
	mkRuntime := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "wb2api"), []byte("bin"), 0700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		_ = os.Chtimes(path, old, old)
		return filepath.Join(path, "wb2api")
	}
	current := mkRuntime("runtime-aaa", 0)
	previous := mkRuntime("runtime-bbb", time.Hour)
	older := mkRuntime("runtime-ccc", 2*time.Hour)
	for _, name := range []string{"v2.4.0-wb2api-linux-arm64", "v2.4.1-wb2api-runtime-linux-arm64.tar.gz", ".download-123.part", "current"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pruneUpdateDir(dir, current)
	for _, want := range []string{filepath.Dir(current), filepath.Dir(previous), filepath.Join(dir, "current")} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("kept path missing: %v", err)
		}
	}
	for _, gone := range []string{older, filepath.Join(dir, "v2.4.0-wb2api-linux-arm64"), filepath.Join(dir, "v2.4.1-wb2api-runtime-linux-arm64.tar.gz")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("expected removal of %s", gone)
		}
	}
}

func TestPruneUpdateDirLeavesForeignFilesAndMissingDirAlone(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	pruneUpdateDir(dir, "")
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("unrelated file was removed")
	}
	pruneUpdateDir(filepath.Join(dir, "missing"), "")
}
