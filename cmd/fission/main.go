// fission.go — Trae 邀请奖励补发 CLI。
//
// 面板上的邀请记录停在「邀请中」，是因为发奖动作在 TRAE 桌面端本地（登录时调
// code_fission/grant）。被邀请人没登过桌面端，记录就永远挂着。本工具把那一次
// 调用搬到服务端补发。
//
// 用法:
//
//	fission                       # 扫描全部 trae 账号，给「有邀请关系」的发奖
//	fission -uid 2081842786229369 # 只处理指定账号（可重复）
//	fission -gen 7                # 用第 7 代设备号（绕过「同一台电脑只能被邀请一次」）
//	fission -dry-run              # 只探测不发放（读 share_link + 当前积分）
//	fission -pretty               # 人类可读输出（默认 JSON）
//
// 退出码：0 全部成功（含 already）；1 有账号失败；2 参数/环境错误。
//
// 输出结构:
//
//	{"service":"trae-fission","ts":N,
//	 "accounts":[{"uid","nickname","code","before","after","delta",
//	              "result":"granted|already|failed","error?"}]}
//
// 2026-10-12 初版：实测 uid 2081842786229369 发放成功（4500→5000，+500）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

type accountResult struct {
	UID      string  `json:"uid"`
	Nickname string  `json:"nickname"`
	Code     string  `json:"code,omitempty"`
	Before   float64 `json:"before"`
	After    float64 `json:"after"`
	Delta    float64 `json:"delta"`
	// Result 三态：granted（本次发放）/ already（此前已发过，幂等成功）/ failed。
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	Gen    int    `json:"gen"`
}

func main() {
	var (
		uids    = uidList{}
		gen     = flag.Int("gen", 0, "设备号代数：0=账号默认设备号，>0=全新设备（绕过「同一台电脑只能被邀请一次」）")
		dryRun  = flag.Bool("dry-run", false, "只探测（查邀请码 + 当前积分），不发放")
		pretty  = flag.Bool("pretty", false, "人类可读输出")
		authDir = flag.String("auths", "", "账号目录（默认取 WB2A_AUTH_DIR 或 ./auths）")
	)
	flag.Var(&uids, "uid", "只处理指定 uid（可重复；省略=全部 trae 账号）")
	flag.Parse()

	dir := *authDir
	if dir == "" {
		dir = os.Getenv("WB2A_AUTH_DIR")
	}
	if dir == "" {
		dir = "./auths"
	}
	if *gen < 0 {
		fmt.Fprintln(os.Stderr, "fission: -gen 不能为负")
		os.Exit(2)
	}

	accounts, err := loadTraeAccounts(dir, uids)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fission:", err)
		os.Exit(2)
	}
	if len(accounts) == 0 {
		fmt.Fprintln(os.Stderr, "fission: 没有匹配的 trae 账号（目录 "+dir+"）")
		os.Exit(2)
	}

	ctx := context.Background()
	results := make([]accountResult, 0, len(accounts))
	failed := 0
	for _, a := range accounts {
		res := runOne(ctx, a, *gen, *dryRun)
		if res.Result == "failed" {
			failed++
		}
		results = append(results, res)
	}

	if *pretty {
		printPretty(results, *dryRun)
	} else {
		out := map[string]any{
			"service":  "trae-fission",
			"ts":       time.Now().Unix(),
			"dry_run":  *dryRun,
			"gen":      *gen,
			"accounts": results,
		}
		raw, _ := json.Marshal(out)
		fmt.Println(string(raw))
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// runOne 处理一个账号：先读发放前积分（dry-run 时只做这一步 + 查邀请码），
// 再发奖，再读发放后积分。前后各查一次是为了让输出能自证「真的到账了」，
// 而不是只凭上游一句 success。
func runOne(ctx context.Context, a *auth.Auth, gen int, dryRun bool) accountResult {
	res := accountResult{UID: a.UID, Nickname: a.Nickname, Gen: gen}

	// 邀请码：既用于核对账号身份，也让 dry-run 有可读产出。
	if code, err := trae.FissionShareLink(ctx, a); err == nil {
		res.Code = code
	}

	before, err := trae.FetchCredits(ctx, a)
	if err != nil {
		res.Result = "failed"
		res.Error = "查积分失败（凭据可能已失效）: " + err.Error()
		return res
	}
	res.Before = before.Total

	if dryRun {
		res.After = before.Total
		res.Result = "granted" // dry-run 不写 result=failed，靠 dry_run 字段区分
		return res
	}

	grant, err := trae.GrantCodeFission(ctx, a, gen)
	if err != nil {
		// 设备号判重：换 gen 重试一次（与签到侧 9074 的处理同构）。
		if trae.IsFissionDeviceUsed(err) {
			retryGen := gen + 1
			grant, err = trae.GrantCodeFission(ctx, a, retryGen)
			res.Gen = retryGen
			if err != nil {
				res.Result = "failed"
				res.Error = err.Error()
				return res
			}
		} else {
			res.Result = "failed"
			res.Error = err.Error()
			return res
		}
	}

	after, aerr := trae.FetchCredits(ctx, a)
	if aerr == nil {
		res.After = after.Total
		res.Delta = after.Total - before.Total
	} else {
		// 发放已成功但复核失败：如实标记，不把 delta 编成 0。
		res.After = before.Total
		res.Error = "发放已调用，但复核积分失败: " + aerr.Error()
	}

	if grant.Already {
		res.Result = "already"
	} else if grant.Granted {
		res.Result = "granted"
	} else {
		res.Result = "failed"
		res.Error = fmt.Sprintf("上游返回未知结果: code %g %s", grant.Code, grant.Message)
	}
	return res
}

// printPretty 人类可读输出：一行一个账号，带前后积分与结果。
func printPretty(results []accountResult, dryRun bool) {
	head := "Trae 邀请奖励补发"
	if dryRun {
		head += "（探测模式，未发放）"
	}
	fmt.Println(head)
	for _, r := range results {
		name := r.Nickname
		if name == "" {
			name = r.UID
		}
		line := fmt.Sprintf("  %-18s %s", name, r.UID)
		if r.Code != "" {
			line += "  码=" + r.Code
		}
		switch r.Result {
		case "granted":
			if dryRun {
				line += fmt.Sprintf("  积分=%.0f", r.Before)
			} else {
				line += fmt.Sprintf("  ✅ 已发放 %.0f → %.0f (+%.0f) gen=%d", r.Before, r.After, r.Delta, r.Gen)
			}
		case "already":
			line += fmt.Sprintf("  ⏭ 此前已发过（积分 %.0f）", r.Before)
		default:
			line += "  ❌ " + r.Error
		}
		fmt.Println(line)
	}
}

// uidList 收集可重复的 -uid 旗标。
type uidList []string

func (u *uidList) String() string { return fmt.Sprint([]string(*u)) }

func (u *uidList) Set(v string) error {
	*u = append(*u, v)
	return nil
}

// loadTraeAccounts 读账号目录，只取 trae 域账号；uids 非空时按 uid 过滤。
//
// 与 cmd/credit 同口径地走 auth.LoadAuthFiles（宽侧 workbuddy*.json），
// 保证「工具看到的账号」与「网关加载的账号」是同一批。
func loadTraeAccounts(dir string, uids []string) ([]*auth.Auth, error) {
	files, err := auth.LoadAuthFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("读账号目录 %s: %w", dir, err)
	}
	want := map[string]bool{}
	for _, u := range uids {
		want[u] = true
	}
	var out []*auth.Auth
	for _, f := range files {
		// #nosec G304,G703 -- 路径来自账号目录扫描
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		a, perr := auth.Parse(raw)
		if perr != nil {
			continue
		}
		if a.Realm() != trae.Realm {
			continue
		}
		if len(want) > 0 && !want[a.UID] {
			continue
		}
		if a.Snapshot().AccessToken == "" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}
