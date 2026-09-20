// ═══ 更新日志 ═══
// 2026-09-20：密钥增加可选有效期，未设置即无限制且既有记录零迁移；到期后鉴权按已过期拒绝并可被调用方区分。
// 2026-09-20：密钥加密保存以支持管理员再次复制，旧密钥在有效鉴权时补齐；增加逐密钥重复推理保护覆盖。
// 2026-09-16：增加持久化多密钥管理，保留原密钥并使启停、删除立即生效，只保存随机密钥的 SHA-256。
// 2026-09-17：密钥可绑定模型白名单；空列表保持不限制，非法模型名拒绝保存。
// 2026-09-18：热更新并存实例按文件版本同步密钥，持锁读改写避免丢失撤销和新建操作；返回策略使用独立副本。
package apikeys

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxKeys = 256
const maxKeyFileBytes = 8 << 20

// MaxBoundModels 单个密钥可绑定的模型数量上限。
const MaxBoundModels = 64

var (
	ErrNotFound      = errors.New("密钥不存在或已删除")
	ErrInvalid       = errors.New("名称需为 1—64 字，备注不超过 256 字，且不能包含控制字符")
	ErrInvalidModels = errors.New("模型绑定需为 1—64 个字符、不含空白或控制字符，且不能重复，最多 64 项")
	ErrInvalidExpiry = errors.New("有效期需为将来时间，且不超过 10 年；留空表示无限制")
	ErrLimit         = errors.New("密钥数量已达上限，请先删除不再使用的密钥")
)

// maxKeyLifetime 限制单个密钥的有效期长度，拦住把毫秒当秒之类的输入错误。
const maxKeyLifetime = 10 * 365 * 24 * time.Hour

// Status 描述一次密钥查询的结果，让调用方能区分「没有这把密钥」与「已过期」。
type Status int

const (
	StatusUnknown Status = iota
	StatusActive
	StatusExpired
)

type Info struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Note          string    `json:"note"`
	MaskedKey     string    `json:"masked_key"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	Legacy        bool      `json:"legacy"`
	CopyAvailable bool      `json:"copy_available,omitempty"`
	// nil follows the server default; a bool overrides it for this key.
	ReasoningLoopGuard *bool `json:"reasoning_loop_guard,omitempty"`
	// Models 该密钥允许调用的模型白名单（裸名或带 realm 前缀）。
	// 空列表表示不限制模型，保持旧密钥零回归。
	Models []string `json:"models"`
	// ExpiresAt 为 nil 表示无限制（既有密钥的默认状态）。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type record struct {
	Info
	Digest       string `json:"sha256"`
	EncryptedKey string `json:"encrypted_key,omitempty"`
}

type Options struct {
	ReasoningLoopGuard *bool
	// ExpiresAt 与 ExpiresAtSet 一起使用：ExpiresAtSet 为 false 表示不改动有效期，
	// 为 true 时 ExpiresAt 为 nil 表示改为无限制。
	ExpiresAt    *time.Time
	ExpiresAtSet bool
}

type document struct {
	Version int      `json:"version"`
	Keys    []record `json:"keys"`
}

type Store struct {
	mu             sync.RWMutex
	path           string
	keys           []record
	persist        func(document) error
	fileInfo       os.FileInfo
	copyRetryAfter time.Time
}

func Open(path, existingKey string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("API key file path is empty")
	}
	s := &Store{path: path, keys: []record{}}
	s.persist = s.write
	unlock, err := lockKeyStore(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	keys, info, err := readKeyRecords(path)
	if errors.Is(err, os.ErrNotExist) {
		if existingKey != "" {
			s.keys = append(s.keys, record{Info: Info{ID: "legacy", Name: "现有密钥", Note: "创建管理页前已在使用，原有客户端可继续使用", MaskedKey: mask(existingKey), Enabled: true, CreatedAt: time.Now().UTC(), Legacy: true}, Digest: digest(existingKey)})
		}
		if err := s.persist(document{Version: 1, Keys: s.keys}); err != nil {
			return nil, err
		}
		s.fileInfo, _ = os.Stat(path)
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open API keys: %w", err)
	}
	s.keys, s.fileInfo = keys, info
	return s, nil
}

func readKeyRecords(path string) ([]record, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxKeyFileBytes {
		return nil, nil, errors.New("API key file exceeds size limit")
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("invalid API key file: %w", err)
	}
	if doc.Version != 1 || len(doc.Keys) > MaxKeys {
		return nil, nil, errors.New("unsupported API key file version or size")
	}
	seenIDs, seenDigests := map[string]bool{}, map[string]bool{}
	for _, key := range doc.Keys {
		decoded, e := hex.DecodeString(key.Digest)
		if e != nil || len(decoded) != sha256.Size || key.ID == "" || strings.ContainsAny(key.ID, "/\\") || seenIDs[key.ID] || seenDigests[key.Digest] || !validLabel(key.Name, key.Note) || !validModels(key.Models) {
			return nil, nil, errors.New("invalid or duplicate API key record")
		}
		seenIDs[key.ID], seenDigests[key.Digest] = true, true
	}
	info, err := f.Stat()
	return doc.Keys, info, err
}

func (s *Store) refreshLocked() error {
	info, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	if s.fileInfo != nil && os.SameFile(s.fileInfo, info) && s.fileInfo.Size() == info.Size() && s.fileInfo.ModTime() == info.ModTime() {
		return nil
	}
	keys, loaded, err := readKeyRecords(s.path)
	if err != nil {
		return err
	}
	s.keys, s.fileInfo = keys, loaded
	return nil
}

// Mutations reread under the same cross-process lock used by writers. A
// draining process must not overwrite a newer instance's key revocations.
func (s *Store) lockAndRefresh() (func(), error) {
	unlock, err := lockKeyStore(s.path)
	if err != nil {
		return nil, err
	}
	if err := s.refreshLocked(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func copyInfo(info Info) Info {
	info.Models = append([]string(nil), info.Models...)
	info.ReasoningLoopGuard = copyBool(info.ReasoningLoopGuard)
	if info.ExpiresAt != nil {
		expiry := *info.ExpiresAt
		info.ExpiresAt = &expiry
	}
	return info
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// validExpiry 校验有效期：nil 表示无限制，其余必须是将来时间且不超过十年。
// 用 UTC 比较，避免调用方传本地时间时出现偏差。
func validExpiry(expiresAt *time.Time, now time.Time) bool {
	if expiresAt == nil {
		return true
	}
	expiry := expiresAt.UTC()
	return expiry.After(now.UTC()) && expiry.Before(now.UTC().Add(maxKeyLifetime))
}

func copyBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func digest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func mask(key string) string {
	if len(key) < 16 {
		return "••••••••"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

func validLabel(name, note string) bool {
	if name != strings.TrimSpace(name) || name == "" || utf8.RuneCountInString(name) > 64 || utf8.RuneCountInString(note) > 256 {
		return false
	}
	for _, r := range name + note {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validModels 校验模型白名单：每项为 1—64 个可见字符，去重后不超过 MaxBoundModels。
// 空列表合法：表示该密钥不限制模型。
func validModels(models []string) bool {
	if len(models) > MaxBoundModels {
		return false
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model != strings.TrimSpace(model) || model == "" || utf8.RuneCountInString(model) > 64 {
			return false
		}
		for _, r := range model {
			if unicode.IsControl(r) || unicode.IsSpace(r) {
				return false
			}
		}
		if seen[model] {
			return false
		}
		seen[model] = true
	}
	return true
}

// normalizeModels 复制并清理入参，保持用户输入顺序。
func normalizeModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, strings.TrimSpace(model))
	}
	return out
}

// Authenticate 只判断密钥是否可用；需要密钥策略时用 Resolve。
func (s *Store) Authenticate(key string) bool {
	_, ok := s.Resolve(key)
	return ok
}

// Resolve 返回可用密钥的信息副本；未命中或已停用时 ok=false。
func (s *Store) Resolve(key string) (Info, bool) {
	info, status := s.Lookup(key)
	return info, status == StatusActive
}

// Lookup 返回密钥信息与状态，让调用方能区分「没有这把密钥」与「密钥已过期」。
// 未启用或已删除按 StatusUnknown 处理，不向调用方泄露密钥是否存在。
func (s *Store) Lookup(key string) (Info, Status) {
	if key == "" || len(key) > 512 {
		return Info{}, StatusUnknown
	}
	want := digest(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return Info{}, StatusUnknown
	}
	for _, entry := range s.keys {
		if entry.Enabled && subtle.ConstantTimeCompare([]byte(want), []byte(entry.Digest)) == 1 {
			info := s.recordInfo(entry)
			if expired(info) {
				return info, StatusExpired
			}
			if !info.CopyAvailable && !time.Now().Before(s.copyRetryAfter) {
				captured, ok := s.captureSecretLocked(entry.ID, key, info)
				if !ok {
					return Info{}, StatusUnknown
				}
				// 补交副本会重新读盘：期间可能刚好到期或被停用，按最新状态判定。
				if expired(captured) {
					return captured, StatusExpired
				}
				return captured, StatusActive
			}
			return info, StatusActive
		}
	}
	return Info{}, StatusUnknown
}

// expired 判定密钥是否已过期；无有效期（nil）表示无限制，永不过期。
func expired(info Info) bool {
	return info.ExpiresAt != nil && !time.Now().Before(*info.ExpiresAt)
}

func (s *Store) List() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return nil
	}
	result := make([]Info, 0, len(s.keys))
	for _, entry := range s.keys {
		result = append(result, s.recordInfo(entry))
	}
	return result
}

func (s *Store) Create(name, note string, models []string, options ...Options) (Info, string, error) {
	name, note = strings.TrimSpace(name), strings.TrimSpace(note)
	models = normalizeModels(models)
	if !validLabel(name, note) {
		return Info{}, "", ErrInvalid
	}
	if !validModels(models) {
		return Info{}, "", ErrInvalidModels
	}
	var raw [32]byte
	var id [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Info{}, "", err
	}
	if _, err := rand.Read(id[:]); err != nil {
		return Info{}, "", err
	}
	key := "wbk_" + base64.RawURLEncoding.EncodeToString(raw[:])
	entry := record{Info: Info{ID: "key_" + hex.EncodeToString(id[:]), Name: name, Note: note, MaskedKey: mask(key), Enabled: true, CreatedAt: time.Now().UTC(), Models: models}, Digest: digest(key)}
	if len(options) > 0 {
		entry.ReasoningLoopGuard = copyBool(options[0].ReasoningLoopGuard)
		if options[0].ExpiresAtSet {
			if !validExpiry(options[0].ExpiresAt, time.Now()) {
				return Info{}, "", ErrInvalidExpiry
			}
			entry.ExpiresAt = copyTime(options[0].ExpiresAt)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockAndRefresh()
	if err != nil {
		return Info{}, "", err
	}
	defer unlock()
	if len(s.keys) >= MaxKeys {
		return Info{}, "", ErrLimit
	}
	entry.EncryptedKey, err = s.sealSecret(entry, key)
	if err != nil {
		return Info{}, "", err
	}
	next := append(append([]record{}, s.keys...), entry)
	if err := s.commit(next); err != nil {
		return Info{}, "", err
	}
	return s.recordInfo(entry), key, nil
}

func (s *Store) Update(id string, name, note *string, enabled *bool, models *[]string, options ...Options) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockAndRefresh()
	if err != nil {
		return Info{}, err
	}
	defer unlock()
	next := append([]record{}, s.keys...)
	for i := range next {
		if next[i].ID != id {
			continue
		}
		if name != nil {
			next[i].Name = strings.TrimSpace(*name)
		}
		if note != nil {
			next[i].Note = strings.TrimSpace(*note)
		}
		if enabled != nil {
			next[i].Enabled = *enabled
		}
		if models != nil {
			next[i].Models = normalizeModels(*models)
		}
		if len(options) > 0 && options[0].ReasoningLoopGuard != nil {
			next[i].ReasoningLoopGuard = copyBool(options[0].ReasoningLoopGuard)
		}
		if len(options) > 0 && options[0].ExpiresAtSet {
			if !validExpiry(options[0].ExpiresAt, time.Now()) {
				return Info{}, ErrInvalidExpiry
			}
			next[i].ExpiresAt = copyTime(options[0].ExpiresAt)
		}
		if !validLabel(next[i].Name, next[i].Note) {
			return Info{}, ErrInvalid
		}
		if !validModels(next[i].Models) {
			return Info{}, ErrInvalidModels
		}
		if err := s.commit(next); err != nil {
			return Info{}, err
		}
		return s.recordInfo(next[i]), nil
	}
	return Info{}, ErrNotFound
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockAndRefresh()
	if err != nil {
		return err
	}
	defer unlock()
	next := make([]record, 0, len(s.keys))
	found := false
	for _, key := range s.keys {
		if key.ID == id {
			found = true
		} else {
			next = append(next, key)
		}
	}
	if !found {
		return ErrNotFound
	}
	return s.commit(next)
}

func (s *Store) commit(keys []record) error {
	if err := s.persist(document{Version: 1, Keys: keys}); err != nil {
		return err
	}
	s.keys = keys
	s.fileInfo, _ = os.Stat(s.path)
	return nil
}

func (s *Store) write(doc document) error {
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(doc); err != nil {
		return err
	}
	if encoded.Len() > maxKeyFileBytes {
		return errors.New("API key file exceeds size limit")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".api-keys-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(encoded.Bytes())
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
