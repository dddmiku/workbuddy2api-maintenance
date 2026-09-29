// ═══ 更新日志 ═══
// 2026-09-25：新增加锁追加、增量读取、有界留存和可见的尾帧恢复，支持热重载新旧进程共同记账。
package requestlog

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const formatVersion = 1

type fileHeader struct {
	Version    int           `json:"version"`
	Generation string        `json:"generation"`
	Recovery   *RecoveryInfo `json:"recovery,omitempty"`
}

type frame struct {
	Record   json.RawMessage `json:"record"`
	Checksum string          `json:"checksum"`
}

type entry struct {
	line    []byte
	summary Summary
}

type snapshot struct {
	header  fileHeader
	entries []entry
	index   map[string]int
	offset  int64
	modTime time.Time
}

// Store is a synchronous, bounded metadata journal. It keeps no data-file
// descriptor open between operations. Every read/append/compaction acquires the
// stable sidecar lock before reopening the current file; stale processes can
// never append to a renamed inode or overwrite a newer snapshot.
type Store struct {
	mu      sync.Mutex
	path    string
	options Options
	now     func() time.Time
	closed  bool
	state   snapshot
}

func Open(path string, options Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("request log path is empty")
	}
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path, options: options, now: time.Now}
	err = s.locked(func() error {
		if err := s.cleanupTempsLocked(); err != nil {
			return err
		}
		if err := s.refreshLocked(false); err != nil {
			return err
		}
		return s.retainLocked()
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Append durably commits one final record. A duplicate ID returns ErrDuplicate
// instead of counting a retry twice. After an I/O error, a commit can be uncertain:
// retry the same ID or query Get; never generate a replacement request ID.
func (s *Store) Append(record Record) error {
	return s.locked(func() error {
		record.RecordedAt = s.now().UTC()
		normalized, err := normalizeRecord(record)
		if err != nil {
			return err
		}
		item, err := encodeEntry(normalized)
		if err != nil {
			return err
		}
		if int64(len(item.line))+512 > s.options.MaxBytes {
			return ErrRecordTooLarge
		}
		if err = s.refreshLocked(false); err != nil {
			return err
		}
		if _, exists := s.state.index[record.RequestID]; exists {
			return ErrDuplicate
		}
		needCompact := s.needsRetention(s.state.entries, s.state.offset) || len(s.state.entries)+1 > s.options.MaxRecords || s.state.offset+int64(len(item.line)) > s.options.MaxBytes
		if needCompact {
			// Validate the complete current file before any replacement. A cached
			// prefix must never hide corruption that a rewrite would otherwise erase.
			if err = s.refreshLocked(true); err != nil {
				return err
			}
			if _, exists := s.state.index[record.RequestID]; exists {
				return ErrDuplicate
			}
			items := append(s.state.entries, item)
			items = s.prune(items, true)
			return s.replaceLocked(items, s.state.header.Recovery)
		}
		file, err := openPrivate(s.path, os.O_WRONLY|os.O_APPEND)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(item.line)
		if writeErr == nil {
			writeErr = file.Sync()
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if writeErr != nil || statErr != nil || closeErr != nil {
			s.state = snapshot{}
			return errors.Join(writeErr, statErr, closeErr)
		}
		s.state.index[item.summary.RequestID] = len(s.state.entries)
		s.state.entries = append(s.state.entries, item)
		s.state.offset = info.Size()
		s.state.modTime = info.ModTime()
		return nil
	})
}

func (s *Store) List(query Query) (Page, error) {
	q, err := normalizeQuery(query)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: []Summary{}, Offset: q.Offset, Limit: q.Limit}
	err = s.locked(func() error {
		if err := s.refreshLocked(false); err != nil {
			return err
		}
		if err := s.retainLocked(); err != nil {
			return err
		}
		if s.state.header.Recovery != nil {
			recovery := *s.state.header.Recovery
			page.Recovery = &recovery
		}
		for i := len(s.state.entries) - 1; i >= 0; i-- {
			r := s.state.entries[i].summary
			if (q.KeyID != "" && r.KeyID != q.KeyID) || (q.Model != "" && r.Model != q.Model) || (q.Status != "" && r.Status != q.Status) || (q.RequestID != "" && r.RequestID != q.RequestID) {
				continue
			}
			if page.Total >= q.Offset && len(page.Items) < q.Limit {
				page.Items = append(page.Items, cloneSummary(r))
			}
			page.Total++
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return page, nil
}

func (s *Store) Get(requestID string) (Record, error) {
	if !validCode(requestID, 128, true) {
		return Record{}, ErrInvalidQuery
	}
	var record Record
	err := s.locked(func() error {
		if err := s.refreshLocked(false); err != nil {
			return err
		}
		if err := s.retainLocked(); err != nil {
			return err
		}
		index, found := s.state.index[requestID]
		if !found {
			return ErrNotFound
		}
		var f frame
		if err := json.Unmarshal(s.state.entries[index].line, &f); err != nil {
			return ErrCorrupt
		}
		if err := json.Unmarshal(f.Record, &record); err != nil {
			return ErrCorrupt
		}
		var err error
		record, err = normalizeRecord(record)
		if err != nil {
			return ErrCorrupt
		}
		return nil
	})
	return record, err
}

// Close waits for an in-progress operation. There is no background flush and no
// pending queue to lose. Repeated calls are safe; later operations return ErrClosed.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.state = snapshot{}
	return nil
}

func (s *Store) locked(fn func() error) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.path == "" {
		return ErrClosed
	}
	unlock, err := lockStore(s.path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func encodeEntry(r Record) (entry, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return entry{}, invalid("encoding")
	}
	line, err := json.Marshal(frame{Record: payload, Checksum: fmt.Sprintf("%08x", crc32.ChecksumIEEE(payload))})
	if err != nil {
		return entry{}, invalid("encoding")
	}
	line = append(line, '\n')
	if len(line) > MaxRecordBytes {
		return entry{}, ErrRecordTooLarge
	}
	return entry{line: line, summary: summarize(r)}, nil
}

func decodeEntry(line []byte) (entry, error) {
	if len(line) > MaxRecordBytes {
		return entry{}, ErrCorrupt
	}
	var f frame
	if err := decodeStrict(line, &f); err != nil {
		return entry{}, ErrCorrupt
	}
	if f.Checksum != fmt.Sprintf("%08x", crc32.ChecksumIEEE(f.Record)) {
		return entry{}, ErrCorrupt
	}
	var r Record
	if err := decodeStrict(f.Record, &r); err != nil {
		return entry{}, ErrCorrupt
	}
	r, err := normalizeRecord(r)
	if err != nil {
		return entry{}, ErrCorrupt
	}
	return entry{line: bytes.Clone(line), summary: summarize(r)}, nil
}

func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrCorrupt
	}
	return nil
}

func (s *Store) refreshLocked(force bool) error {
	state, tailBytes, usedCache, err := s.readCurrentLocked(force)
	if errors.Is(err, os.ErrNotExist) {
		// An intentionally removed file is a fresh journal, never a request to
		// resurrect the old process's cached history.
		s.state = snapshot{}
		return s.replaceLocked(nil, nil)
	}
	if err != nil {
		s.state = snapshot{}
		return err
	}
	if tailBytes > 0 {
		if usedCache {
			state, tailBytes, _, err = s.readCurrentLocked(true)
			if err != nil {
				s.state = snapshot{}
				return err
			}
		}
		recovery := RecoveryInfo{}
		if state.header.Recovery != nil {
			recovery = *state.header.Recovery
		}
		if recovery.Count >= 1<<40 || recovery.DiscardedTailBytes > (1<<53)-tailBytes {
			s.state = snapshot{}
			return fmt.Errorf("%w: recovery counters exhausted", ErrCorrupt)
		}
		recovery.Count++
		recovery.DiscardedTailBytes += tailBytes
		recovery.LastRecoveredAt = s.now().UTC()
		return s.replaceLocked(s.prune(state.entries, false), &recovery)
	}
	s.state = state
	return nil
}

// readCurrentLocked never repairs on its own. Only an invalid final frame can
// be discarded; invalid headers/middle frames and duplicate IDs fail closed.
func (s *Store) readCurrentLocked(force bool) (snapshot, int64, bool, error) {
	file, err := openPrivate(s.path, os.O_RDONLY)
	if err != nil {
		return snapshot{}, 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return snapshot{}, 0, false, err
	}
	if info.Size() > DefaultMaxBytes+MaxRecordBytes {
		return snapshot{}, 0, false, fmt.Errorf("%w: file size", ErrCorrupt)
	}
	reader := bufio.NewReaderSize(file, MaxRecordBytes+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 1024 {
		return snapshot{}, 0, false, fmt.Errorf("%w: header", ErrCorrupt)
	}
	var header fileHeader
	if err := decodeStrict(line, &header); err != nil {
		return snapshot{}, 0, false, fmt.Errorf("%w: header", ErrCorrupt)
	}
	if header.Version != formatVersion || len(header.Generation) != 32 {
		return snapshot{}, 0, false, fmt.Errorf("%w: version or generation", ErrCorrupt)
	}
	if _, err := hex.DecodeString(header.Generation); err != nil {
		return snapshot{}, 0, false, fmt.Errorf("%w: generation", ErrCorrupt)
	}
	if header.Recovery != nil && (header.Recovery.Count < 1 || header.Recovery.Count > 1<<40 || header.Recovery.DiscardedTailBytes < 1 || header.Recovery.DiscardedTailBytes > 1<<53 || !validTime(header.Recovery.LastRecoveredAt)) {
		return snapshot{}, 0, false, fmt.Errorf("%w: recovery metadata", ErrCorrupt)
	}
	state := snapshot{header: header, index: make(map[string]int), offset: int64(len(line)), modTime: info.ModTime()}
	useCache := !force && s.state.header.Generation == header.Generation && info.Size() >= s.state.offset && (info.Size() != s.state.offset || info.ModTime().Equal(s.state.modTime))
	if useCache {
		state = s.state
		state.header, state.modTime = header, info.ModTime()
		if _, err := file.Seek(state.offset, io.SeekStart); err != nil {
			return snapshot{}, 0, false, err
		}
		reader.Reset(file)
	}
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return state, 0, useCache, nil
			}
			return state, info.Size() - state.offset, useCache, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = reader.ReadSlice('\n')
			}
			if errors.Is(err, io.EOF) || (err == nil && readerAtEnd(reader)) {
				return state, info.Size() - state.offset, useCache, nil
			}
			return snapshot{}, 0, false, fmt.Errorf("%w: oversized middle frame", ErrCorrupt)
		}
		if err != nil {
			return snapshot{}, 0, false, err
		}
		item, err := decodeEntry(line)
		if err != nil {
			if readerAtEnd(reader) {
				return state, info.Size() - state.offset, useCache, nil
			}
			return snapshot{}, 0, false, fmt.Errorf("%w: middle frame at byte %d", ErrCorrupt, state.offset)
		}
		if _, exists := state.index[item.summary.RequestID]; exists {
			return snapshot{}, 0, false, fmt.Errorf("%w: duplicate request id", ErrCorrupt)
		}
		if len(state.entries) >= DefaultMaxRecords {
			return snapshot{}, 0, false, fmt.Errorf("%w: record count", ErrCorrupt)
		}
		state.index[item.summary.RequestID] = len(state.entries)
		state.entries = append(state.entries, item)
		state.offset += int64(len(line))
	}
}

func readerAtEnd(reader *bufio.Reader) bool {
	_, err := reader.Peek(1)
	return errors.Is(err, io.EOF)
}

func (s *Store) needsRetention(items []entry, size int64) bool {
	if len(items) > s.options.MaxRecords || size > s.options.MaxBytes {
		return true
	}
	cutoff := s.now().Add(-s.options.MaxAge)
	for _, item := range items {
		if !item.summary.RecordedAt.After(cutoff) {
			return true
		}
	}
	return false
}

func (s *Store) retainLocked() error {
	if !s.needsRetention(s.state.entries, s.state.offset) {
		return nil
	}
	if err := s.refreshLocked(true); err != nil {
		return err
	}
	return s.replaceLocked(s.prune(s.state.entries, false), s.state.header.Recovery)
}

func (s *Store) prune(items []entry, batch bool) []entry {
	cutoff := s.now().Add(-s.options.MaxAge)
	live := make([]entry, 0, len(items))
	var size int64 = 512 // reserves space for the header and recovery metadata
	for _, item := range items {
		if item.summary.RecordedAt.After(cutoff) {
			live = append(live, item)
			size += int64(len(item.line))
		}
	}
	countLimit, byteLimit := s.options.MaxRecords, s.options.MaxBytes
	if batch && len(live) > countLimit {
		countLimit = max(1, countLimit-max(1, countLimit/10))
	}
	if batch && size > byteLimit {
		byteLimit = max(512, byteLimit-byteLimit/10)
	}
	start := 0
	for start < len(live)-1 && (len(live)-start > countLimit || size > byteLimit) {
		size -= int64(len(live[start].line))
		start++
	}
	return live[start:]
}

func (s *Store) replaceLocked(items []entry, recovery *RecoveryInfo) error {
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return err
	}
	header := fileHeader{Version: formatVersion, Generation: hex.EncodeToString(generation[:]), Recovery: recovery}
	line, err := json.Marshal(header)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	var size = int64(len(line))
	for _, item := range items {
		size += int64(len(item.line))
	}
	if size > s.options.MaxBytes || len(items) > s.options.MaxRecords {
		return ErrRecordTooLarge
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), s.tempPrefix()+"*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(line)
	}
	for _, item := range items {
		if err != nil {
			break
		}
		_, err = file.Write(item.line)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		s.state = snapshot{}
		return errors.Join(err, closeErr)
	}
	if err = replaceJournal(temporary, s.path); err != nil {
		s.state = snapshot{}
		return err
	}
	if err = syncDirectory(filepath.Dir(s.path)); err != nil {
		s.state = snapshot{}
		return err
	}
	info, err := os.Stat(s.path)
	if err != nil {
		s.state = snapshot{}
		return err
	}
	// Copy the slice so evicted frames are not retained through its backing array.
	state := snapshot{header: header, entries: append([]entry{}, items...), index: make(map[string]int, len(items)), offset: size, modTime: info.ModTime()}
	for i, item := range state.entries {
		state.index[item.summary.RequestID] = i
	}
	s.state = state
	return nil
}

func cloneSummary(s Summary) Summary {
	for _, value := range []**int64{&s.InputTokens, &s.OutputTokens, &s.CachedTokens, &s.ReasoningTokens, &s.TTFBMS} {
		if *value != nil {
			copy := **value
			*value = &copy
		}
	}
	if s.Credit != nil {
		copy := *s.Credit
		s.Credit = &copy
	}
	if s.LastDecision != nil {
		copy := *s.LastDecision
		s.LastDecision = &copy
	}
	return s
}

func (s *Store) tempPrefix() string {
	digest := sha256.Sum256([]byte(s.path))
	return ".requestlog-" + hex.EncodeToString(digest[:8]) + "-"
}

// The stable journal lock ensures no current writer can own these temporary
// files. Only this exact path's generated regular files are removed, so crashes
// cannot accumulate snapshots and another journal's active compaction is safe.
func (s *Store) cleanupTempsLocked() error {
	files, err := os.ReadDir(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	prefix := s.tempPrefix()
	for _, file := range files {
		if !strings.HasPrefix(file.Name(), prefix) || !strings.HasSuffix(file.Name(), ".tmp") || !file.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(filepath.Dir(s.path), file.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// MaxFacets 限制返回的筛选项条数：管理通道有界，且下拉本身不适合放太多项。
const MaxFacets = 500

// Facets 返回去重后的可筛选调用密钥与模型名，按最近出现顺序（新在前）。
//
// 只遍历内存快照，不重读文件；与 List 共用同一把锁与刷新路径，因此与分页
// 结果看到的是同一份状态。达到 MaxFacets 后停止收集并置 Truncated，如实
// 说明列表被截断，而不是让调用方以为这就是全部。
func (s *Store) Facets() (Facets, error) {
	out := Facets{Keys: []FacetKey{}, Models: []string{}}
	seenKey := make(map[string]bool)
	seenModel := make(map[string]bool)
	err := s.locked(func() error {
		if err := s.refreshLocked(false); err != nil {
			return err
		}
		if err := s.retainLocked(); err != nil {
			return err
		}
		for i := len(s.state.entries) - 1; i >= 0; i-- {
			r := s.state.entries[i].summary
			if r.KeyID != "" && !seenKey[r.KeyID] {
				if len(out.Keys) >= MaxFacets {
					out.Truncated = true
					return nil
				}
				seenKey[r.KeyID] = true
				out.Keys = append(out.Keys, FacetKey{ID: r.KeyID, Name: r.KeyName})
			}
			if r.Model != "" && !seenModel[r.Model] {
				if len(out.Models) >= MaxFacets {
					out.Truncated = true
					return nil
				}
				seenModel[r.Model] = true
				out.Models = append(out.Models, r.Model)
			}
		}
		return nil
	})
	if err != nil {
		return Facets{}, err
	}
	return out, nil
}
