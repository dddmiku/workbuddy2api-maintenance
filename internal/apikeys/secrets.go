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
func (s *Store) vaultCipher(create bool) (cipher.AEAD, error) {
	path := s.path + ".enc-key"
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
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(block)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrSecretUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer f.Close()
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		return nil, ErrSecretUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	return cipher.NewGCM(block)
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
