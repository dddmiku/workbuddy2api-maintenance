// ═══ 更新日志 ═══
// 2026-09-25：验证尾行恢复、损坏中间行保留证据、跨压缩读写及链接/权限边界。
package requestlog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCorruptTailRecoveryIsDurableAndObservable(t *testing.T) {
	for name, tail := range map[string][]byte{
		"partial-json":         []byte(`{"record":{"request_id":"partial`),
		"malformed-final-line": []byte("{broken}\n"),
		"oversized-final-line": []byte(strings.Repeat("x", MaxRecordBytes+100) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requests.jsonl")
			s := openFixture(t, path, Options{})
			appendFixture(t, s, fixtureRecord("first"))
			appendFixture(t, s, fixtureRecord("second"))
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(tail); err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
			// This path deliberately uses a warm cache to exercise full revalidation
			// before recovery, not just the straightforward new-process read.
			page, err := s.List(Query{})
			if err != nil || page.Total != 2 {
				t.Fatalf("valid prefix not recovered: %v %+v", err, page)
			}
			if page.Recovery == nil || page.Recovery.Count != 1 || page.Recovery.DiscardedTailBytes != int64(len(tail)) {
				t.Fatalf("recovery was hidden: %+v", page.Recovery)
			}
			appendFixture(t, s, fixtureRecord("third"))
			reopened := openFixture(t, path, Options{})
			page, err = reopened.List(Query{})
			if err != nil || page.Total != 3 || page.Recovery == nil || page.Recovery.Count != 1 {
				t.Fatalf("recovered state was not durable: %v %+v", err, page)
			}
			page.Recovery.Count = 99
			page, _ = reopened.List(Query{})
			if page.Recovery.Count != 1 {
				t.Fatal("recovery status aliases caller")
			}
		})
	}
}

func TestMiddleCorruptionAndDuplicateFramesFailWithoutOverwrite(t *testing.T) {
	for _, name := range []string{"middle", "checksum-middle", "header", "version", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requests.jsonl")
			s := openFixture(t, path, Options{})
			appendFixture(t, s, fixtureRecord("first"))
			appendFixture(t, s, fixtureRecord("second"))
			raw, _ := os.ReadFile(path)
			lines := bytes.SplitAfter(raw, []byte{'\n'})
			var broken []byte
			switch name {
			case "middle":
				broken = append(append(append([]byte{}, lines[0]...), []byte("{broken}\n")...), lines[2]...)
			case "checksum-middle":
				broken = bytes.Replace(raw, []byte(`"request_id":"first"`), []byte(`"request_id":"other"`), 1)
			case "header":
				broken = append([]byte("{broken}\n"), raw[len(lines[0]):]...)
			case "version":
				broken = bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1)
			case "duplicate":
				broken = append(append([]byte{}, raw...), lines[1]...)
			}
			if err := os.WriteFile(path, broken, 0o600); err != nil {
				t.Fatal(err)
			}
			// Force same-size external corruption to be visible on coarse timestamp
			// filesystems as well; it is not a cooperating append.
			future := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatal(err)
			}
			if _, err := s.List(Query{}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corruption was ignored: %v", err)
			}
			if err := s.Append(fixtureRecord("must-not-write")); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corruption was overwritten: %v", err)
			}
			if _, err := Open(path, Options{}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("new process ignored corruption: %v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, broken) {
				t.Fatal("original corrupt evidence was changed")
			}
		})
	}
}

func TestCompactionRevalidatesPreviouslyCachedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{MaxRecords: 2})
	appendFixture(t, s, fixtureRecord("first"))
	appendFixture(t, s, fixtureRecord("second"))
	raw, _ := os.ReadFile(path)
	broken := bytes.Replace(raw, []byte(`"request_id":"first"`), []byte(`"request_id":"other"`), 1)
	// Keep the cached generation, file size and timestamp to model an external
	// in-place edit. The incremental path cannot see it, but replacement must.
	info, _ := os.Stat(path)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(fixtureRecord("third")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("compaction trusted a stale prefix: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, broken) {
		t.Fatal("compaction erased corruption")
	}
}

func TestDeletedJournalNeverResurrectsCachedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	a, b := openFixture(t, path, Options{}), openFixture(t, path, Options{})
	appendFixture(t, a, fixtureRecord("old"))
	if _, err := b.Get("old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendFixture(t, a, fixtureRecord("new"))
	appendFixture(t, b, fixtureRecord("also-new"))
	page, err := a.List(Query{})
	if err != nil || page.Total != 2 {
		t.Fatalf("fresh journal failed: %v %+v", err, page)
	}
	if _, err := b.Get("old"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old process resurrected removed history")
	}
}

func TestJournalAndLockRejectLinksOrSpecialFiles(t *testing.T) {
	for _, name := range []string{"symlink-journal", "symlink-lock", "hardlink-journal", "hardlink-lock", "directory"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path, target := filepath.Join(dir, "requests.jsonl"), filepath.Join(dir, "unchanged.txt")
			if err := os.WriteFile(target, []byte("unchanged fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			leaf := path
			if strings.HasSuffix(name, "lock") {
				leaf += ".lock"
			}
			var err error
			switch {
			case strings.HasPrefix(name, "symlink"):
				err = os.Symlink(target, leaf)
			case strings.HasPrefix(name, "hardlink"):
				err = os.Link(target, leaf)
			default:
				err = os.Mkdir(leaf, 0o700)
			}
			if err != nil && strings.HasPrefix(name, "symlink") && runtime.GOOS == "windows" {
				t.Skipf("Windows symlink privilege unavailable: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, Options{}); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("unsafe leaf accepted: %v", err)
			}
			data, _ := os.ReadFile(target)
			if string(data) != "unchanged fixture" {
				t.Fatal("link target was modified")
			}
		})
	}
}

func TestOwnerOnlyFilesIncludingAfterCompaction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not Windows ACLs")
	}
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "requests.jsonl")
	s := openFixture(t, path, Options{MaxRecords: 2})
	for _, id := range []string{"first", "second", "third"} {
		appendFixture(t, s, fixtureRecord(id))
	}
	for _, path := range []string{dir, path, path + ".lock"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("private mode missing for %s: %v", path, err)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(Query{}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatal("existing loose file was not tightened")
	}
}

func TestRecoveryCanTightenRetentionWithoutLosingNewestRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	for _, id := range []string{"one", "two", "three", "four", "five"} {
		appendFixture(t, s, fixtureRecord(id))
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{incomplete")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	reopened := openFixture(t, path, Options{MaxRecords: 2})
	page, err := reopened.List(Query{})
	if err != nil || page.Total > 2 || page.Recovery == nil {
		t.Fatalf("recovery with tighter bounds failed: %v %+v", err, page)
	}
	if _, err := reopened.Get("five"); err != nil {
		t.Fatal("newest committed record was lost")
	}
}

func TestCompleteJSONWithoutFinalNewlineIsUncommitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	s := openFixture(t, path, Options{})
	appendFixture(t, s, fixtureRecord("committed"))
	r := fixtureRecord("uncommitted")
	r.RecordedAt = time.Now().UTC()
	r, err := normalizeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	item, err := encodeEntry(r)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write(item.line[:len(item.line)-1])
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.List(Query{})
	if err != nil || page.Total != 1 || page.Recovery == nil {
		t.Fatalf("unterminated frame was committed: %v %+v", err, page)
	}
	if _, err := s.Get("uncommitted"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unterminated record appeared in details")
	}
}

func TestAbandonedSnapshotCleanupOnlyTouchesThisJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.jsonl")
	s := openFixture(t, path, Options{})
	orphan := filepath.Join(dir, s.tempPrefix()+"orphan.tmp")
	unrelated := filepath.Join(dir, ".requestlog-other-journal-orphan.tmp")
	special := filepath.Join(dir, s.tempPrefix()+"directory.tmp")
	for _, name := range []string{orphan, unrelated} {
		if err := os.WriteFile(name, []byte("test snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(special, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = openFixture(t, path, Options{})
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned snapshot was retained")
	}
	for _, name := range []string{unrelated, special} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("cleanup touched unrelated path: %v", err)
		}
	}
}
