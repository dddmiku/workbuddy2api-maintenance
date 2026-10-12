// ═══ 更新日志 ═══
// 2026-10-13：初版——Go 版口令哈希，与 panel/app.py 的两种形态逐字兼容。
//
// 两种形态都必须支持，缺一不可：
//
//  1. pbkdf2_sha256$200000$<b64salt>$<b64hash>  —— 本面板自己写的新口令；
//  2. $apr1$<salt>$<hash>                       —— 从 nginx .htpasswd 继承的存量口令。
//
// 第 2 种是历史包袱但**不能丢**：面板接手前密码由 nginx basic auth 管，
// 老部署的 credentials.json 里存的就是 apr1 哈希。少支持一种，那些部署的用户
// 就再也登不进面板了（而且没有「重新设置密码」的入口——要先登录才能改）。
//
// apr1 是 Apache 的 MD5-crypt 变体，标准库没有，必须手写。实现逐行对照
// Python 版与 `openssl passwd -apr1`，并有测试用固定向量锁住。
package panel

import (
	"crypto/md5" // #nosec G501 -- apr1 是历史遗留哈希，必须兼容存量凭据；新口令一律用 PBKDF2
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// PBKDF2Rounds 与 Python 版一致。调高会让老哈希校验变慢但不失败（轮数写在串里）。
const PBKDF2Rounds = 200000

// itoa64 是 apr1 的自定义 base64 字母表（不是标准 base64）。
const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// HashPassword 生成 pbkdf2_sha256 形态的口令串。
//
// 输出必须与 Python 的 "%s$%d$%s$%s" % (prefix, rounds, b64(salt), b64(hash)) 相同：
// 用的是**标准 base64（带 padding）**，不是 URL-safe、也不是 Raw。
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, PBKDF2Rounds, sha256.Size)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%s", PBKDF2Rounds,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 校验明文口令。stored 支持 pbkdf2_sha256$… 与 $apr1$… 两种。
//
// 未知形态一律返回 false（不报错）：调用方只关心「对不对」，且把「哈希格式不认识」
// 当成错误会让登录接口回显内部细节。
func VerifyPassword(password, stored string) bool {
	if stored == "" {
		return false
	}
	if strings.HasPrefix(stored, "$apr1$") {
		parts := strings.Split(stored, "$")
		// "$apr1$" + salt + "$" + hash → Split 得到 ["", "apr1", salt, hash]
		if len(parts) < 4 || len(parts[2]) < 8 {
			return false
		}
		salt := parts[2][:8]
		got := apr1Crypt(password, salt)
		return subtle.ConstantTimeCompare([]byte(got), []byte(stored)) == 1
	}
	if strings.HasPrefix(stored, "pbkdf2_sha256$") {
		parts := strings.SplitN(stored, "$", 4)
		if len(parts) != 4 {
			return false
		}
		rounds, err := strconv.Atoi(parts[1])
		if err != nil || rounds <= 0 {
			return false
		}
		salt, err := base64.StdEncoding.DecodeString(parts[2])
		if err != nil {
			return false
		}
		want, err := base64.StdEncoding.DecodeString(parts[3])
		if err != nil {
			return false
		}
		got, err := pbkdf2.Key(sha256.New, password, salt, rounds, len(want))
		if err != nil {
			return false
		}
		return subtle.ConstantTimeCompare(got, want) == 1
	}
	return false
}

// apr1Crypt 实现 Apache apr1（MD5-crypt）。
//
// 与 `htpasswd -m` / `openssl passwd -apr1` 等价。算法步骤逐行对照 Python 版，
// 任何一步顺序错了都会得到「格式正确但永远校验失败」的哈希——那种 bug 在
// 真实登录里表现为「密码明明对却登不进」，且只对继承来的老账号出现。
func apr1Crypt(password, salt string) string {
	pw := []byte(password)
	sl := []byte(salt)
	if len(sl) > 8 {
		sl = sl[:8]
	}
	magic := []byte("$apr1$")

	// 第一轮：md5(pw + magic + salt)
	ctx := md5.New() // #nosec G401 -- apr1 兼容性要求，见文件头注释
	ctx.Write(pw)
	ctx.Write(magic)
	ctx.Write(sl)

	// alt = md5(pw + salt + pw)
	alt := md5.New() // #nosec G401 -- 同上
	alt.Write(pw)
	alt.Write(sl)
	alt.Write(pw)
	altSum := alt.Sum(nil)

	// 把 alt 按 pw 长度重复填入（每次最多 16 字节）
	for i := len(pw); i > 0; i -= 16 {
		if i > 16 {
			ctx.Write(altSum[:16])
		} else {
			ctx.Write(altSum[:i])
		}
	}
	// 按 pw 长度的二进制位追加：0 位写 pw 首字节，1 位写 \x00
	for i := len(pw); i != 0; i >>= 1 {
		if i&1 != 0 {
			ctx.Write([]byte{0})
		} else {
			ctx.Write(pw[:1])
		}
	}
	digest := ctx.Sum(nil)

	// 1000 轮强化
	for i := 0; i < 1000; i++ {
		c := md5.New() // #nosec G401 -- 同上
		if i&1 != 0 {
			c.Write(pw)
		} else {
			c.Write(digest)
		}
		if i%3 != 0 {
			c.Write(sl)
		}
		if i%7 != 0 {
			c.Write(pw)
		}
		if i&1 != 0 {
			c.Write(digest)
		} else {
			c.Write(pw)
		}
		digest = c.Sum(nil)
	}

	// 输出：按固定顺序把 24 字节摘要编码成 22 个字符
	var out strings.Builder
	for _, group := range [][3]int{{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5}} {
		value := int(digest[group[0]])<<16 | int(digest[group[1]])<<8 | int(digest[group[2]])
		out.WriteString(to64(value, 4))
	}
	out.WriteString(to64(int(digest[11]), 2))
	return "$apr1$" + salt[:min(8, len(salt))] + "$" + out.String()
}

// to64 把 value 的低位按 6 位一组编码成 count 个字符（apr1 的小端编码）。
func to64(value, count int) string {
	var out strings.Builder
	for i := 0; i < count; i++ {
		out.WriteByte(itoa64[value&0x3F])
		value >>= 6
	}
	return out.String()
}

// ParseHtpasswd 读 .htpasswd，返回第一条有效记录的 (用户名, 原始哈希)。
//
// 找不到或读不了时返回空串——调用方据此走「生成新凭证」分支，而不是报错中断。
func ParseHtpasswd(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, ":") {
			continue
		}
		name, digest, _ := strings.Cut(line, ":")
		name, digest = strings.TrimSpace(name), strings.TrimSpace(digest)
		if name != "" && digest != "" {
			return name, digest
		}
	}
	return "", ""
}

// ErrWeakPassword 口令太短（登录页与改密接口共用同一判据）。
var ErrWeakPassword = errors.New("新口令至少 8 位")
