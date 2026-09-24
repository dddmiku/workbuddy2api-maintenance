// ═══ 更新日志 ═══
// 2026-09-25：多实例共享日志写入不丢行，轮换后文件数量和体积保持有界。
package runlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSharedWritersPreserveRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	var group sync.WaitGroup
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			w := &Writer{path: path}
			for j := 0; j < 50; j++ {
				if _, err := w.Write([]byte("one row\n")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "one row\n") != 200 {
		t.Fatal("concurrent logs lost rows")
	}
}

func TestRunlogRetentionBound(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{path: filepath.Join(dir, "gateway.log")}
	data := bytes.Repeat([]byte("x"), int(MaxBytes)+100)
	for i := 0; i < 5; i++ {
		if n, err := w.Write(data); err != nil || n != len(data) {
			t.Fatalf("write %d %v", n, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) > 4 {
		t.Fatal("unbounded retained files")
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Size() > MaxBytes {
			t.Fatal("file exceeds log size bound")
		}
	}
}
