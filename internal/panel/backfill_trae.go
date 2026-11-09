// 2026-11-09：Trae 账号手机号回填。
//
// ═══ 问题 ═══
//
// Trae 的 uid 是一串内部数字（如 411685466945619），昵称是上游生成的
// 「用户6126656924」——两者都看不出这是哪个号。唯一能把账号和「人」对上的
// 是掩码手机号（138******00），它只能在**添加账号的那一刻**由 GetUserInfo 取到，
// 写进 account.mobile。
//
// 2026-11-09 实测：dev 上 6 个 Trae 账号的 account.mobile **全部缺失**——
// 它们是在这个字段引入之前加的（userinfo.go 写于 2026-10-29），
// 而写入路径只在 writeTraeAuthFile 里，回填从来没做过。于是面板只能显示
// 「用户99028827821」这种上游昵称。
//
// ═══ 修法 ═══
//
// 对缺 mobile 的 Trae 账号补一次 GetUserInfo，把 mobile 与真实 ScreenName
// 写回账号文件。已有值一律不动，避免重复打上游触发频控。
package panel

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

// BackfillTraeProfiles 给缺 mobile 的 Trae 账号补手机号与昵称。
//
// 返回实际更新条数与问题列表。单个账号失败不影响其他账号——这是尽力而为的
// 补数据，不是关键路径。
func (s *Server) BackfillTraeProfiles(ctx context.Context) (int, []string) {
	if ctx == nil {
		ctx = context.Background()
	}
	entries := s.store.List()
	updated := 0
	var problems []string
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return updated, append(problems, "已取消")
		default:
		}
		doc := readAuthDoc(entry.Path)
		if len(doc) == 0 {
			continue
		}
		authSec, _ := doc["auth"].(map[string]any)
		realm := firstString(authSec, "realm")
		if realm == "" {
			realm = firstString(doc, "realm")
		}
		if realm != "trae" {
			continue
		}
		acctSec, _ := doc["account"].(map[string]any)
		if acctSec == nil {
			continue
		}
		// 已经有手机号就不再查：省一次上游往返，也避开频控。
		if firstString(acctSec, "mobile") != "" {
			continue
		}
		token := firstString(authSec, "accessToken")
		if token == "" {
			continue
		}
		info, err := trae.GetUserInfo(ctx, &auth.Auth{UID: entry.UID, AccessToken: token, Domain: "api.trae.cn"})
		if err != nil {
			problems = append(problems, entry.UID+": "+err.Error())
			continue
		}
		if info.Mobile == "" {
			// 上游没给手机号（纯邮箱注册的号）：不改文件，避免每次启动都重试。
			continue
		}
		acctSec["mobile"] = info.Mobile
		if name := strings.TrimSpace(info.ScreenName); name != "" {
			acctSec["nickname"] = name
		}
		doc["account"] = acctSec
		if err := writeAuthDocAtomic(entry.Path, doc); err != nil {
			problems = append(problems, entry.UID+": 写入失败 "+err.Error())
			continue
		}
		updated++
		log.Printf("INFO: [面板] Trae 账号已补手机号 uid=%s mobile=%s", shortUID(entry.UID), info.Mobile)
	}
	return updated, problems
}

// writeAuthDocAtomic 原子写回账号文件（先写临时文件再 rename）。
//
// 直接 WriteFile 会在进程被杀时留下半个 JSON，那个账号就再也加载不了。
func writeAuthDocAtomic(path string, doc map[string]any) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp-backfill"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// shortUID 取 uid 前 8 位用于日志（完整 uid 15-16 位，日志里太长）。
func shortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}
