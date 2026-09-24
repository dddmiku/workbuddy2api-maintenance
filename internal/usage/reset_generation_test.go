// ═══ 更新日志 ═══
// 2026-09-24：复现热更新重叠实例的旧快照让清零后的密钥、日期和起算时间重新出现。
package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResetStaysVisibleToIdleOverlappingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	current, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = current.Close() })
	before := time.Now().UTC()
	current.Record("old-key", "old name", "mask", "old-model", 10, 3, 0, 0, false, before)
	if err := current.Flush(); err != nil {
		t.Fatal(err)
	}
	previous, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previous.Close() })
	resetAt := before.Add(time.Minute)
	if err := current.Reset(resetAt); err != nil {
		t.Fatal(err)
	}
	view := previous.Snapshot()
	if !view.Since.Equal(resetAt) || view.Totals.Requests != 0 || len(view.Keys) != 0 || len(view.Days) != 0 {
		t.Fatalf("idle process resurrected reset metadata: since=%v keys=%d days=%d totals=%+v", view.Since, len(view.Keys), len(view.Days), view.Totals)
	}
	previous.Record("new-key", "new name", "mask", "new-model", 7, 2, 0, 0, false, resetAt.Add(time.Minute))
	if err := previous.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*Store{current, previous} {
		view = store.Snapshot()
		if !view.Since.Equal(resetAt) || view.Totals.Requests != 1 || len(view.Keys) != 1 || view.Keys[0].KeyID != "new-key" {
			t.Fatalf("new request restored old records after reset: since=%v keys=%+v totals=%+v", view.Since, view.Keys, view.Totals)
		}
	}
}
