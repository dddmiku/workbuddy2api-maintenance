// ═══ 更新日志 ═══
// 2026-10-13：初版——Go 版面板后端的会话与凭据层，与 panel/app.py 的格式逐字兼容。
//
// 为什么要 Go 版：安卓 App 需要单二进制（手机上装 CPython 代价太大），Windows 桌面版
// 也能因此摆脱「必须装 Python」。前端（index.html / *.js）完全复用，只换后端。
//
// ═══ 兼容性是硬约束 ═══
//
// 已有部署的 credentials.json 与浏览器里的登录 cookie 都必须是**可继续使用**的：
//   - sessionKey 是 64 位 hex，落盘在 credentials.json；
//   - 会话令牌是 `base64url(payload) + "." + base64url(HMAC-SHA256(key, body))`；
//   - payload 是 {"u":用户名,"e":过期秒,"n":12 字节 hex}，JSON 无空格。
//
// 任何一处不一致（比如 JSON 里多了空格、base64 带 padding）都会让所有已登录用户
// 被登出，且症状是「登录页反复要求重新登录」这种很难定位的现象。所以这里的
// 序列化与编码必须与 Python 版**逐字节相同**，并有测试锁住。
package panel

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SessionTTL 与 Python 版一致：30 天。管理台是长期挂着的运维界面。
const SessionTTL = 30 * 24 * time.Hour

// Credentials 是 credentials.json 的结构。
//
// 用 map 而不是 struct 是为了**保住未知字段**：Python 版会把整个 doc 读进来、
// 改几个键、再整体写回。若 Go 侧用 struct 解析再序列化，用户手动加的字段
// （或未来版本加的字段）会被静默丢掉。运维文件被工具悄悄改写是危险的。
type Credentials map[string]any

var (
	// ErrCredentialsInvalid 凭证文件存在但内容不合法。
	ErrCredentialsInvalid = errors.New("登录凭证文件无效，请恢复已有备份")

	hex64Re   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	nonceRe   = regexp.MustCompile(`^[0-9a-f]{24}$`)
	pbkdf2Re  = regexp.MustCompile(`^pbkdf2_sha256\$`)
	credMutex sync.Mutex
)

// CredentialStore 管理 credentials.json 的读写。
type CredentialStore struct {
	Path string
}

// Load 读取凭证；文件不存在时生成一份新的（随机口令写进 initial-password.txt）。
func (s *CredentialStore) Load() (Credentials, error) {
	credMutex.Lock()
	defer credMutex.Unlock()

	raw, err := os.ReadFile(s.Path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return s.createInitial()
	}
	var doc Credentials
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, ErrCredentialsInvalid
	}
	username, _ := doc["username"].(string)
	password, _ := doc["password"].(string)
	if username == "" || password == "" {
		return nil, ErrCredentialsInvalid
	}
	// 缺 sessionKey 时补一个（与 Python 版一致：老文件平滑升级）。
	key, _ := doc["sessionKey"].(string)
	if key == "" {
		generated, err := randomHex(32)
		if err != nil {
			return nil, err
		}
		doc["sessionKey"] = generated
		if err := s.saveLocked(doc); err != nil {
			return nil, err
		}
		key = generated
	}
	if !hex64Re.MatchString(key) {
		return nil, ErrCredentialsInvalid
	}
	if revoked, ok := doc["revokedSessions"]; ok {
		if _, isMap := revoked.(map[string]any); !isMap {
			return nil, ErrCredentialsInvalid
		}
	}
	return doc, nil
}

// Save 原子写回凭证（保留未知字段）。
func (s *CredentialStore) Save(doc Credentials) error {
	credMutex.Lock()
	defer credMutex.Unlock()
	return s.saveLocked(doc)
}

// saveLocked 原子写：临时文件 + rename，权限 0600。
//
// 不用「截断再写」：进程在写入中途被杀会留下半个 JSON，用户就再也登不进面板了。
func (s *CredentialStore) saveLocked(doc Credentials) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".credentials-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这里是 no-op
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.Path)
}

// createInitial 没有凭证文件时的兜底：随机口令 + initial-password.txt。
func (s *CredentialStore) createInitial() (Credentials, error) {
	password, err := randomURLSafe(12)
	if err != nil {
		return nil, err
	}
	hashed, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	key, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	doc := Credentials{
		"version":    1,
		"username":   "wbadmin",
		"password":   hashed,
		"sessionKey": key,
		"updatedAt":  time.Now().Unix(),
		"history":    []any{},
	}
	if err := s.saveLocked(doc); err != nil {
		return nil, err
	}
	hint := filepath.Join(filepath.Dir(s.Path), "initial-password.txt")
	content := fmt.Sprintf("username: wbadmin\npassword: %s\n", password)
	if err := os.WriteFile(hint, []byte(content), 0600); err == nil {
		fmt.Fprintf(os.Stderr, "未找到凭证，已生成初始账号，见 %s\n", hint)
	}
	return doc, nil
}

// SessionKey 取出并校验 64 位 hex 的会话密钥。
func (c Credentials) SessionKey() ([]byte, error) {
	key, _ := c["sessionKey"].(string)
	if !hex64Re.MatchString(key) {
		return nil, ErrCredentialsInvalid
	}
	return hex.DecodeString(key)
}

func (c Credentials) Username() string {
	name, _ := c["username"].(string)
	return name
}

func (c Credentials) Password() string {
	pw, _ := c["password"].(string)
	return pw
}

// IssueSession 签发会话令牌。
//
// 格式：base64url(JSON{"u","e","n"}) + "." + base64url(HMAC-SHA256(key, body))
//
// JSON 必须**无空格**（Python 用 separators=(",", ":")）：Go 的 json.Marshal 默认
// 也不加空格，两者一致。字段顺序按结构体声明顺序，与 Python 的插入顺序
// u → e → n 相同——否则 body 不同、签名对不上。
func IssueSession(creds Credentials, nonce string, ttl time.Duration) (string, error) {
	return IssueSessionAt(creds, nonce, ttl, time.Now())
}

// IssueSessionAt 与 IssueSession 相同，但用调用方给的时刻算过期。
//
// 为什么要一个显式时刻的版本：面板的其他部分（限流窗口、冷却剩余、滑动续期判断）
// 都走注入的时钟。如果签发/校验仍读 time.Now()，测试里推进假时钟后会出现
// 「令牌按真实时间算过期、按假时间判有效性」的错位——那种错位在生产里不会出现，
// 却会让「续期、过期、撤销」这三条最容易出错的路径无法被确定性测试覆盖。
func IssueSessionAt(creds Credentials, nonce string, ttl time.Duration, now time.Time) (string, error) {
	if nonce == "" {
		var err error
		if nonce, err = randomHex(12); err != nil {
			return "", err
		}
	}
	key, err := creds.SessionKey()
	if err != nil {
		return "", err
	}
	payload := struct {
		U string `json:"u"`
		E int64  `json:"e"`
		N string `json:"n"`
	}{U: creds.Username(), E: now.Add(ttl).Unix(), N: nonce}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig, nil
}

// SessionPayload 是校验通过的会话内容。
type SessionPayload struct {
	Username string
	Expires  int64
	Nonce    string
}

// ReadSession 校验会话令牌；无效返回 nil（不返回错误——调用方只关心「有没有登录」）。
func ReadSession(creds Credentials, token string) *SessionPayload {
	return ReadSessionAt(creds, token, time.Now())
}

// ReadSessionAt 与 ReadSession 相同，但用调用方给的时刻判断过期（理由见 IssueSessionAt）。
func ReadSessionAt(creds Credentials, token string, now time.Time) *SessionPayload {
	if token == "" || !strings.Contains(token, ".") {
		return nil
	}
	body, sig, _ := strings.Cut(token, ".")
	key, err := creds.SessionKey()
	if err != nil {
		return nil
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil
	}
	// 定长比较：避免时序侧信道泄露签名前缀。
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil
	}
	var payload struct {
		U string `json:"u"`
		E any    `json:"e"`
		N string `json:"n"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if !nonceRe.MatchString(payload.N) {
		return nil
	}
	expires, ok := toInt64(payload.E)
	if !ok || expires < now.Unix() {
		return nil
	}
	if payload.U != creds.Username() {
		return nil
	}
	if revokedNonce(creds, payload.N) {
		return nil
	}
	return &SessionPayload{Username: payload.U, Expires: expires, Nonce: payload.N}
}

// revokedNonce 检查该 nonce 是否已被撤销（登出）。
//
// 撤销记录落盘在 credentials.json 的 revokedSessions 里，所以面板重启后
// 「已登出的会话」不会复活。
func revokedNonce(creds Credentials, nonce string) bool {
	revoked, _ := creds["revokedSessions"].(map[string]any)
	if revoked == nil {
		return false
	}
	_, found := revoked[nonce]
	return found
}

// RevokeSession 把一个 nonce 记入撤销表并落盘。
func (s *CredentialStore) RevokeSession(creds Credentials, nonce string) error {
	revoked, _ := creds["revokedSessions"].(map[string]any)
	if revoked == nil {
		revoked = map[string]any{}
		creds["revokedSessions"] = revoked
	}
	revoked[nonce] = time.Now().Unix()
	return s.Save(creds)
}

// toInt64 兼容 JSON 数字的两种解码形态（float64 与 json.Number）。
func toInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
