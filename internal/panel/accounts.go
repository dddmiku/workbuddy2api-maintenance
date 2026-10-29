// ═══ 更新日志 ═══
// 2026-10-13：初版——账号凭据文件的读写层（列表/摘要/启停/删除），与 panel/app.py 同口径。
//
// 账号文件是面板与网关共享的事实来源：面板按它渲染列表、启停、删除，网关按它
// 加载账号。两边口径必须一致，否则会出现「面板显示已停用、网关仍在用」这类
// 互相矛盾的状态。
//
// ═══ 两个必须对齐的口径 ═══
//
//  1. **文件命名**：workbuddy<uid>.json，停用是加 .disabled 后缀（不是移到别处）。
//     网关扫目录时按同一正则识别，所以停用必须是「改名」而不是「搬走」。
//
//  2. **uid 的来源**：优先取**文件内容**里的 uid，取不到才回落到文件名。
//     网关按内容里的 uid 建索引，只靠文件名会在无连字符命名
//     （workbuddy_new.json）下与网关对不上，导致启停/删除打空。
//
// 落盘格式有两种形态，都必须能读：嵌套形（account/auth 两段）与扁平形
// （顶层 accessToken/uid/realm，手写或旧版）。与网关 auth.Parse 同口径。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// authFileRe 匹配账号文件名：workbuddy<uid>.json[.disabled]
var authFileRe = regexp.MustCompile(`^workbuddy(?P<uid>.*)\.json(?P<disabled>\.disabled)?$`)

// uidRe 是 uid 的合法性判据，与 panel/app.py 的 UID_RE 逐字相同。
//
// 它是账号类接口的**唯一**入参闸门：uid 会拼进文件名（启停要改名）、拼进回收件名、
// 还会作为参数传给 ./fission_bin。放宽一格就等于把路径穿越引进来，所以这里与
// Python 侧保持一致、不额外放宽（80 字符上限来自上游 uid 的实际长度）。
var uidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// AuthEntry 是账号文件在列表里的一条。
type AuthEntry struct {
	UID      string
	File     string
	Disabled bool
	Path     string
}

// AuthSummary 是面板渲染账号行所需的信息（与 Python 版 auth_summary 的键一一对应）。
type AuthSummary struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterpriseId"`
	Realm        string `json:"realm"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expiresAt"`
	Disabled     bool   `json:"disabled"`
	Parsable     bool   `json:"parsable"`
	// Mobile 是掩码手机号（Trae 账号才有，形如 138******00）。
	//
	// 落盘位置：账号文件的 account.mobile。由添加账号时的 GetUserInfo 写入，
	// 面板只读不查——每次刷新都打 6 次上游接口会把页面拖慢 1-2 秒，
	// 而手机号几乎不变。
	Mobile string `json:"mobile,omitempty"`
}

// AccountStore 管理 auths 目录。
type AccountStore struct {
	AuthDir  string
	TrashDir string
}

// List 列出全部账号文件，按文件名排序（保证面板行序稳定）。
func (s *AccountStore) List() []AuthEntry {
	entries, err := os.ReadDir(s.AuthDir)
	if err != nil {
		return nil
	}
	items := make([]AuthEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		m := authFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		path := filepath.Join(s.AuthDir, name)
		raw := readAuthDoc(path)
		uid := authDocUID(raw)
		if uid == "" {
			// 回落到文件名中间段（去掉惯用的连字符前缀）。
			uid = strings.TrimLeft(m[1], "-")
		}
		items = append(items, AuthEntry{
			UID:      uid,
			File:     name,
			Disabled: m[2] != "",
			Path:     path,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].File < items[j].File })
	return items
}

// Find 按 uid 找账号；uid 形态不合法时返回 nil（不报错）。
func (s *AccountStore) Find(uid string) *AuthEntry {
	if !uidRe.MatchString(uid) {
		return nil
	}
	for _, entry := range s.List() {
		if entry.UID == uid {
			found := entry
			return &found
		}
	}
	return nil
}

// Summary 生成账号行的展示信息。
//
// 昵称回退链：nickname → account.uid → uid 前 8 位。中间那一档是给「有 uid 但
// 没设昵称」的账号用的，少了它这类账号在面板上会显示成空白。
func Summary(entry AuthEntry) AuthSummary {
	raw := readAuthDoc(entry.Path)
	// 兼容两种落盘形态：嵌套形取 auth/account 段，扁平形两段都指向顶层。
	auth := raw
	if nested, ok := raw["auth"].(map[string]any); ok {
		auth = nested
	}
	acct := raw
	if section, ok := raw["account"].(map[string]any); ok {
		acct = section
	}
	out := AuthSummary{
		UID:      entry.UID,
		Disabled: entry.Disabled,
		Parsable: len(raw) > 0,
	}
	if name := firstString(acct, "nickname"); name != "" {
		out.Nickname = name
	} else if id := firstString(acct, "uid"); id != "" {
		out.Nickname = id
	} else if len(entry.UID) >= 8 {
		out.Nickname = entry.UID[:8]
	} else {
		out.Nickname = entry.UID
	}
	out.EnterpriseID = firstString(acct, "enterpriseId")
	// 手机号：Trae 账号由 GetUserInfo 写入 account.mobile（掩码形态）。
	// 旧账号没有这个字段，留空由前端回落到 uid 显示。
	out.Mobile = firstString(acct, "mobile")
	out.Domain = firstString(auth, "domain")
	// realm 缺失时留空串（不是 "cn"）：面板用 realmOf() 兜底成 cn，
	// 在这里硬填会让「未标注域」与「明确 cn」无法区分。
	out.Realm = firstString(auth, "realm")
	if out.Realm == "" {
		out.Realm = firstString(raw, "realm")
	}
	out.ExpiresAt = toInt64Lenient(auth["expiresAt"])
	return out
}

// Toggle 启用/停用账号：改文件名后缀，不改内容。
//
// 为什么用改名而不是搬目录：网关扫的是 auths 目录，改名让它立刻看不见这个账号，
// 同时文件还在原地（运维能直接看到「这个号被停用了」）。搬走会让人以为文件丢了。
func (s *AccountStore) Toggle(uid string, disabled bool) error {
	entry := s.Find(uid)
	if entry == nil {
		return fmt.Errorf("找不到账号 %s", uid)
	}
	if entry.Disabled == disabled {
		return ErrNoChange
	}
	src := entry.Path
	var dst string
	if disabled {
		dst = src + ".disabled"
	} else {
		dst = strings.TrimSuffix(src, ".disabled")
	}
	if _, err := os.Stat(dst); err == nil {
		// 同时存在启用与禁用两份：保留两份文件让人核对，不擅自覆盖。
		return errors.New("存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号")
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	chownApp(dst)
	return nil
}

// ErrNoChange 状态本来就对，无需改动（调用方据此显示「状态未变化」）。
var ErrNoChange = errors.New("状态未变化")

// Delete 把账号移入回收目录（可手动恢复），不直接删除。
//
// ⚠️ 必须容忍**跨文件系统**的搬运：容器里 auths 与 auths-trash 是两个**独立的 bind
// mount**（/app/auths、/app/auths-trash），rename(2) 跨挂载点必然返回 EXDEV
// （"invalid cross-device link"）。2026-10-15 用户实测删号报的就是这个错。
//
// 判据用 os.Rename 的真实错误（errors.Is(err, syscall.EXDEV)）而不是「先比较 device
// 号」：device 号比较要额外 stat 两次且在某些网络文件系统上不可靠，而 rename 本来
// 就是我们要的语义——成功即最优路径（原子、零拷贝），失败再回落。
//
// 回落路径是「复制 + fsync + 删除源」：复制到回收目录里的临时名，落盘后再改名成
// 最终名，最后才删源文件。顺序很重要——先删源会让中途失败变成「账号凭空消失」，
// 而先落地目标则最坏情况是「两份都在」，用户核对后自行删一份即可（能移不删）。
func (s *AccountStore) Delete(uid string) (string, error) {
	entry := s.Find(uid)
	if entry == nil {
		return "", fmt.Errorf("找不到账号 %s", uid)
	}
	if err := os.MkdirAll(s.TrashDir, 0700); err != nil {
		return "", err
	}
	stamp := time.Now().Format("20060102-150405")
	suffix, err := randomHex(6)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(s.TrashDir, fmt.Sprintf("%s.%s.%s", entry.File, stamp, suffix))
	if err := os.Rename(entry.Path, dst); err != nil {
		if !isCrossDevice(err) {
			return "", err
		}
		if err := copyFileAcross(entry.Path, dst); err != nil {
			return "", err
		}
		// 目标已落盘（含 fsync），现在才删源。删不掉不算失败：目标已经在回收目录里，
		// 账号「被删掉」的效果已达成，源文件残留只是多一份副本，下次删除会再处理。
		_ = os.Remove(entry.Path)
	}
	chownApp(dst)
	return filepath.Base(dst), nil
}

// isCrossDevice 报告错误是否为「跨文件系统重命名」（EXDEV）。
//
// 实现在 exdev_unix.go / exdev_windows.go 里按平台分文件：syscall.EXDEV 只存在于
// Unix 平台（Windows 上引用它会编译失败），而 Windows 上的对应错误码是
// ERROR_NOT_SAME_DEVICE(17)，同样要识别——桌面版把 auths 与 auths-trash 放在
// 不同盘符/挂载点时是同一个故障。

// copyFileAcross 跨文件系统搬运一个文件：复制到目标 → fsync → 关闭，保留权限。
//
// 用 0600 落盘：凭据文件含 access token，任何时刻都不该比原来更宽松。
// 不用 io.Copy 的默认缓冲之外的花样——文件只有几 KB，一次读完即可。
func copyFileAcross(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	mode := os.FileMode(0600)
	if info, statErr := os.Stat(src); statErr == nil {
		mode = info.Mode().Perm()
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := out.Write(raw); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	// fsync 后再返回：回收件是「删号后的唯一副本」，不能只在页缓存里。
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// readAuthDoc 读账号文件；读不到或不是对象时返回空 map（调用方按「不可解析」处理）。
func readAuthDoc(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return map[string]any{}
	}
	return doc
}

// authDocUID 从内容取 uid，兼容嵌套形与扁平形。
func authDocUID(raw map[string]any) string {
	if acct, ok := raw["account"].(map[string]any); ok {
		if uid := firstString(acct, "uid"); uid != "" {
			return uid
		}
	}
	return firstString(raw, "uid")
}

// firstString 取第一个非空字符串字段。
func firstString(doc map[string]any, key string) string {
	if doc == nil {
		return ""
	}
	value, _ := doc[key].(string)
	return strings.TrimSpace(value)
}

// toInt64Lenient 把 JSON 里的数字宽松转成 int64；转不了返回 0。
//
// 用宽松转换而不是报错：expiresAt 是展示用的（「剩 N 天」），个别账号字段
// 畸形不该让整行渲染失败。
func toInt64Lenient(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return n
	case string:
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err != nil {
			return 0
		}
		return n
	}
	return 0
}
