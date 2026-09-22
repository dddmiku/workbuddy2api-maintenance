// ═══ 更新日志 ═══
// 2026-09-20：用 AES-256-GCM 保存可复制副本，密钥与记录绑定；复制失败不影响原有摘要鉴权。
package apikeys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrSecretNotStored = errors.New("这把旧密钥尚未保存完整内容，请先正常使用一次后再复制")
var ErrSecretUnavailable = errors.New("密钥副本暂不可读取，请检查加密备份；密钥状态未改变")

func (s *Store) recordInfo(entry record) Info {
	info := copyInfo(entry.Info)
	info.CopyAvailable = false
	if entry.EncryptedKey != "" {
		_, err := s.openSecret(entry)
		info.CopyAvailable = err == nil
	}
	return info
}

// Secret is only exposed through the local management handler. Disabled keys
// can be copied by the administrator; copying never changes authorization state.
func (s *Store) Secret(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockAndRefresh()
	if err != nil {
		return "", err
	}
	defer unlock()
	for _, entry := range s.keys {
		if entry.ID == id {
			return s.openSecret(entry)
		}
	}
	return "", ErrNotFound
}

// create=true is only called while holding the key registry's cross-process
// lock. A missing key must not be replaced while encrypted records still exist.
//
// 调用方须持 s.mu（全部调用点都已持锁），因此可以直接复用/更新缓存字段。
func (s *Store) vaultCipher(create bool) (cipher.AEAD, error) {
	path := s.path + ".enc-key"
	// 热路径：TTL 内直接用缓存，连 stat 都省掉。副本校验在每次 Lookup 都会走这里，
	// 而主密钥只在轮换时变化；轮换后用旧密钥解密会 AEAD 认证失败（表现为副本暂不可
	// 读），不会误放行，所以这点延迟不构成安全边界。
	if !create && len(s.vaultKey) == 32 && time.Since(s.vaultCheckedAt) < vaultKeyTTL {
		return s.vaultAEAD()
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		for _, entry := range s.keys {
			if entry.EncryptedKey != "" {
				return nil, ErrSecretUnavailable
			}
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// #nosec G304 -- 主密钥路径 = 密钥库路径 + .enc-key，非请求输入
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, ErrSecretUnavailable
		}
		if _, err = f.Write(key); err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			_ = os.Remove(path)
			return nil, ErrSecretUnavailable
		}
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
		// 刚建好的主密钥同样进缓存，省掉紧随其后的第一次读盘。
		s.vaultKey, s.vaultInfo = append([]byte(nil), key...), nil
		if fresh, statErr := os.Lstat(path); statErr == nil {
			s.vaultInfo = fresh
		}
		s.vaultCheckedAt = time.Now()
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		s.vaultKey, s.vaultInfo = nil, nil
		return nil, ErrSecretUnavailable
	}
	key, err := s.vaultKeyLocked(path, info)
	if err != nil {
		return nil, err
	}
	s.vaultCheckedAt = time.Now()
	return s.vaultAEADFor(key)
}

// vaultKeyTTL 是副本主密钥文件的核对间隔。主密钥轮换后最多这么久才会被重新读取；
// 期间旧密钥解密会 AEAD 认证失败（副本暂不可读），不会误放行任何密钥。
const vaultKeyTTL = 30 * time.Second

// vaultAEAD 用缓存的主密钥构造 AEAD（调用方须持 s.mu 且缓存有效）。
func (s *Store) vaultAEAD() (cipher.AEAD, error) {
	return s.vaultAEADFor(s.vaultKey)
}

func (s *Store) vaultAEADFor(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	return cipher.NewGCM(block)
}

// vaultKeyLocked 返回副本主密钥，命中缓存时不做任何 I/O。
// 判据与 refreshLocked 一致：同一 inode、同大小、同 mtime 才复用。
func (s *Store) vaultKeyLocked(path string, info os.FileInfo) ([]byte, error) {
	if len(s.vaultKey) == 32 && s.vaultInfo != nil && os.SameFile(s.vaultInfo, info) &&
		s.vaultInfo.Size() == info.Size() && s.vaultInfo.ModTime() == info.ModTime() {
		return s.vaultKey, nil
	}
	// #nosec G304 -- 同上：读取主密钥
	f, err := os.Open(path)
	if err != nil {
		s.vaultKey, s.vaultInfo = nil, nil
		return nil, ErrSecretUnavailable
	}
	defer f.Close()
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		s.vaultKey, s.vaultInfo = nil, nil
		return nil, ErrSecretUnavailable
	}
	s.vaultKey, s.vaultInfo = key, info
	return key, nil
}

func secretAAD(entry record) []byte {
	return []byte("workbuddy2api-copy-v1\x00" + entry.ID + "\x00" + entry.Digest)
}

func (s *Store) sealSecret(entry record, secret string) (string, error) {
	aead, err := s.vaultCipher(true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(secret), secretAAD(entry))
	return "v1." + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (s *Store) openSecret(entry record) (string, error) {
	if entry.EncryptedKey == "" {
		return "", ErrSecretNotStored
	}
	if !strings.HasPrefix(entry.EncryptedKey, "v1.") || len(entry.EncryptedKey) > 2048 {
		return "", ErrSecretUnavailable
	}
	aead, err := s.vaultCipher(false)
	if err != nil {
		return "", ErrSecretUnavailable
	}
	sealed, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(entry.EncryptedKey, "v1."))
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", ErrSecretUnavailable
	}
	nonce := sealed[:aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, sealed[aead.NonceSize():], secretAAD(entry))
	if err != nil || len(plain) == 0 || len(plain) > 512 {
		return "", ErrSecretUnavailable
	}
	secret := string(plain)
	if subtle.ConstantTimeCompare([]byte(digest(secret)), []byte(entry.Digest)) != 1 {
		return "", ErrSecretUnavailable
	}
	return secret, nil
}

func (s *Store) captureSecretLocked(id, secret string, authenticated Info) (Info, bool) {
	unlock, err := s.lockAndRefresh()
	if err != nil {
		s.copyRetryAfter = time.Now().Add(time.Minute)
		return authenticated, true
	}
	defer unlock()
	for i, entry := range s.keys {
		if entry.ID != id {
			continue
		}
		// A revoke/update may have completed between the initial authentication
		// read and the cross-process write lock. Never resurrect stale policy.
		if !entry.Enabled || subtle.ConstantTimeCompare([]byte(digest(secret)), []byte(entry.Digest)) != 1 {
			return Info{}, false
		}
		info := s.recordInfo(entry)
		if info.CopyAvailable {
			return info, true
		}
		sealed, err := s.sealSecret(entry, secret)
		if err == nil {
			next := append([]record{}, s.keys...)
			next[i].EncryptedKey = sealed
			err = s.commit(next)
			if err == nil {
				return s.recordInfo(next[i]), true
			}
		}
		s.copyRetryAfter = time.Now().Add(time.Minute)
		log.Printf("WARN: [api-keys] encrypted copy could not be saved; digest authentication remains available")
		return info, true
	}
	return Info{}, false
}
