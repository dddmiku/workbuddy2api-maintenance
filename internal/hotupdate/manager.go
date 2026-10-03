// ═══ 更新日志 ═══
// 2026-09-26：下载前验签、下载后与签名摘要对账，不符即拒绝更新。
// 2026-09-26：交接完成后清理更新目录。
// 2026-09-24：自动更新不降级；管理员显式指定当前 latest 的旧标签时保留手动回滚能力。
// 2026-09-18：新实例就绪后才原子提交重启指针，提交失败终止候选实例，并保留原启动参数。
// 2026-09-17：新增热更新管理器：查版本、下载校验、监听套接字交接、优雅停机，
//
//	并把状态暴露给管理台。全过程不中断在途请求（含长 SSE 对话）。
package hotupdate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/version"
)

// ExitHandover 交接完成后旧进程的退出码。
//
// 约定：容器里的 PID 1（docker-entrypoint.sh）看到这个退出码就不再拉起新实例——
// 此刻监听套接字已经在新进程手里，再起一个只会撞端口；同时 PID 1 保持存活，
// 容器不会因为主进程退出而被销毁。
const ExitHandover = 75

// State 更新状态机。
type State string

const (
	StateIdle        State = "idle"
	StateChecking    State = "checking"
	StateDownloading State = "downloading"
	StateHandover    State = "handover"
	StateFailed      State = "failed"
)

// Status 对外暴露的更新状态。
type Status struct {
	Enabled bool `json:"enabled"`
	// Visibility 发布仓库可见性（public/private/unknown）。热更新是否可用由它决定：
	// 公开仓库匿名可读发布，热更新可用；私有仓库需要令牌，未配令牌时明确不可用。
	Visibility Visibility `json:"visibility"`
	// UnavailableReason 不可用时的原因（给面板与运维看的中文说明）；可用时为空。
	UnavailableReason string    `json:"unavailable_reason,omitempty"`
	Current           string    `json:"current"`
	Commit            string    `json:"commit"`
	BuiltAt           string    `json:"built_at"`
	Repo              string    `json:"repo"`
	Dir               string    `json:"dir"`
	State             State     `json:"state"`
	LatestTag         string    `json:"latest_tag"`
	LatestAt          time.Time `json:"latest_at"`
	UpdateReady       bool      `json:"update_ready"`
	AssetName         string    `json:"asset_name"`
	AssetSize         int64     `json:"asset_size"`
	CheckedAt         time.Time `json:"checked_at"`
	LastError         string    `json:"last_error"`
	InheritedFD       bool      `json:"inherited_fd"`
}

// Options 构造参数。
type Options struct {
	Bundle   bool
	Enabled  bool
	Repo     string
	Token    string
	Dir      string
	Binary   string   // 当前可执行文件路径（作为新实例的候选；实际用下载件）
	Args     []string // 新实例命令行参数（沿用本进程）
	Listener net.Listener
	// AdminListener 管理 socket（Unix domain）。新实例没法重新 bind 同一个路径，
	// 必须一并继承；nil = 本次部署没有管理通道。
	AdminListener net.Listener
	OnSwitched    func() // 交接成功后的回调（main 用它触发优雅停机）
}

// Manager 热更新管理器。
type Manager struct {
	opts   Options
	client *Client

	mu         sync.Mutex
	state      State
	latest     Release
	checkedAt  time.Time
	lastError  string
	busy       bool
	visibility Visibility
}

// NewManager 构造管理器；Dir 为空时回落到系统临时目录下的固定子目录。
func NewManager(opts Options) *Manager {
	if strings.TrimSpace(opts.Dir) == "" {
		opts.Dir = filepath.Join(os.TempDir(), "wb2api-updates")
	}
	manager := &Manager{
		opts:   opts,
		client: NewClient(opts.Repo, opts.Token),
		state:  StateIdle,
	}
	manager.client.Bundle = opts.Bundle
	return manager
}

// Status 返回当前状态快照。
func (m *Manager) Status() Status {
	if m == nil {
		return Status{State: StateIdle}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

// availabilityLocked 报告当前是否可用热更新，以及不可用的原因。
//
// 可用性由「发布仓库是否开放」决定，而不是靠人工维护一个与仓库状态同步的开关：
//   - 公开仓库：匿名就能读到 Release（含 SHA256SUMS 与签名），可用；
//   - 私有仓库：匿名读不到，必须配 update.token 才可用；
//   - 未确认：探测失败，先不声称可用（如实报告，不猜）。
//
// config update.enabled 仍是总开关：显式关掉时一律不可用。
func (m *Manager) availabilityLocked() (bool, string) {
	if m == nil {
		return false, "热更新未初始化"
	}
	if !m.opts.Enabled {
		return false, "热更新已在配置中关闭（config update.enabled=false）"
	}
	switch m.visibility {
	case VisibilityPublic:
		return true, ""
	case VisibilityPrivate:
		if strings.TrimSpace(m.client.Token) != "" {
			return true, ""
		}
		return false, "发布仓库已转为私有，匿名读不到 Release；把仓库改回公开，或配置 update.token"
	default:
		return false, "尚未确认发布仓库可见性，请稍后重试「检查更新」"
	}
}

// RefreshVisibility 探测并记录发布仓库可见性。启动时后台调用一次，Check/Apply 各再确认一次。
func (m *Manager) RefreshVisibility(ctx context.Context) Visibility {
	if m == nil {
		return VisibilityUnknown
	}
	visibility := m.client.RepoVisibility(ctx)
	m.mu.Lock()
	m.visibility = visibility
	m.mu.Unlock()
	return visibility
}

// Check 查询最新版本并更新状态。
func (m *Manager) Check(ctx context.Context) (Status, error) {
	if m == nil || !m.opts.Enabled {
		return m.Status(), errors.New("自更新未启用（config update.enabled=false）")
	}
	if visibility := m.RefreshVisibility(ctx); visibility != VisibilityPublic {
		m.mu.Lock()
		available, reason := m.availabilityLocked()
		m.mu.Unlock()
		if !available {
			err := errors.New(reason)
			m.fail(err)
			return m.Status(), err
		}
	}
	m.mu.Lock()
	if m.busy {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, errors.New("已有更新任务在进行中")
	}
	m.busy = true
	m.state = StateChecking
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	release, err := m.client.Latest(ctx)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	m.mu.Lock()
	m.latest = release
	m.checkedAt = time.Now().UTC()
	m.lastError = ""
	m.state = StateIdle
	status := m.statusLocked()
	m.mu.Unlock()
	return status, nil
}

// Apply 执行更新：下载校验后把监听套接字交给新实例，成功时回调 OnSwitched。
//
// 调用方应异步调用（下载与交接耗时以秒计）。返回前的最后一步是交接：
// 新实例报告就绪后本进程才开始优雅停机，在途请求继续跑完。
func (m *Manager) Apply(ctx context.Context, target string) (Status, error) {
	if m == nil || !m.opts.Enabled {
		return m.Status(), errors.New("自更新未启用（config update.enabled=false）")
	}
	if visibility := m.RefreshVisibility(ctx); visibility != VisibilityPublic {
		m.mu.Lock()
		available, reason := m.availabilityLocked()
		m.mu.Unlock()
		if !available {
			err := errors.New(reason)
			m.fail(err)
			return m.Status(), err
		}
	}
	m.mu.Lock()
	if m.busy {
		status := m.statusLocked()
		m.mu.Unlock()
		return status, errors.New("已有更新任务在进行中")
	}
	if m.opts.Listener == nil {
		m.mu.Unlock()
		return m.Status(), errors.New("没有可交接的监听套接字（热更新需要以正常启动方式运行）")
	}
	m.busy = true
	m.state = StateChecking
	m.lastError = ""
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()

	release := Release{Tag: target}
	if strings.TrimSpace(target) == "" {
		found, err := m.client.Latest(ctx)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		release = found
	} else {
		// 指定版本：先查最新确认资产可用，再按需比对。
		found, err := m.client.Latest(ctx)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		if found.Tag != target {
			err = fmt.Errorf("指定版本 %s 不是最新发布（最新为 %s）；本功能只支持升级到最新版", target, found.Tag)
			m.fail(err)
			return m.Status(), err
		}
		release = found
	}
	if release.Tag == "" {
		err := errors.New("远端没有可用发布")
		m.fail(err)
		return m.Status(), err
	}
	sameVersion := strings.TrimPrefix(release.Tag, "v") == strings.TrimPrefix(strings.TrimSpace(version.Version), "v")
	if sameVersion || (strings.TrimSpace(target) == "" && !release.UpdateAvailable()) {
		err := fmt.Errorf("没有可自动升级的版本（当前 %s，最新发布 %s）", version.Version, release.Tag)
		m.fail(err)
		return m.Status(), err
	}
	if release.AssetURL == "" && release.AssetAPIURL == "" {
		err := fmt.Errorf("发布 %s 没有本机架构的二进制资产（%s）", release.Tag, release.AssetName)
		m.fail(err)
		return m.Status(), err
	}
	if strings.TrimSpace(release.Digest) == "" {
		err := fmt.Errorf("发布 %s 的资产没有 sha256 摘要，拒绝执行未校验的二进制", release.Tag)
		m.fail(err)
		return m.Status(), err
	}

	// 发布签名校验：拿到内置公钥时，必须验签通过才继续下载二进制。
	// 没有签名或签名不符一律拒绝——这是自更新的信任根，不能用「拿不到就放行」兜底。
	signedDigest, verified, err := m.client.VerifySignedRelease(ctx, release)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	if verified {
		log.Printf("[update] release %s signature verified (sha256:%s)", release.Tag, signedDigest[:12])
	} else {
		log.Printf("WARN: [update] this build has no release signing key; skipping signature verification for %s", release.Tag)
	}

	m.mu.Lock()
	m.latest = release
	m.state = StateDownloading
	m.mu.Unlock()

	path, sum, err := m.client.Download(ctx, release, m.opts.Dir)
	if err != nil {
		m.fail(err)
		return m.Status(), err
	}
	log.Printf("[update] downloaded %s (%s, sha256=%s)", release.Tag, path, sum[:12])
	// 已验签时，落盘摘要必须与签名清单一致（Download 校验的是 GitHub 的 digest）。
	if verified && !strings.EqualFold(sum, signedDigest) {
		_ = os.Remove(path)
		err := fmt.Errorf("下载资产与签名清单不一致（实际 %s ≠ 签名 %s）", sum[:12], signedDigest[:12])
		m.fail(err)
		return m.Status(), err
	}
	stage := ""
	committed := false
	defer func() {
		if stage != "" && !committed {
			_ = os.RemoveAll(stage)
		}
	}()
	if m.opts.Bundle {
		path, err = extractRuntimeBundle(path, release, m.opts.Dir)
		if err != nil {
			m.fail(err)
			return m.Status(), err
		}
		stage = filepath.Dir(path)
	}

	m.mu.Lock()
	m.state = StateHandover
	m.mu.Unlock()

	if err := handover(path, m.opts.Args, m.opts.Listener, m.opts.AdminListener, readyTimeout, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 就绪和重启指针共同构成提交点：任一步失败仍由旧实例服务。
		if err := writeCurrentPointer(m.opts.Dir, path); err != nil {
			return fmt.Errorf("write current pointer: %w", err)
		}
		return nil
	}); err != nil {
		m.fail(err)
		return m.Status(), err
	}
	log.Printf("[update] handover to %s done; draining in-flight requests", release.Tag)
	committed = true
	// 交接已提交：清理旧运行目录与下载包（保留当前与上一个）。
	pruneUpdateDir(m.opts.Dir, path)
	m.mu.Lock()
	m.state = StateIdle
	m.checkedAt = time.Now().UTC()
	m.mu.Unlock()
	if m.opts.OnSwitched != nil {
		m.opts.OnSwitched()
	}
	return m.Status(), nil
}

// Reload uses the same ready/rollback/drain contract as updates, but keeps the
// current complete runtime. It is only exposed through the local admin socket.
func (m *Manager) Reload(ctx context.Context) error {
	if m == nil {
		return errors.New("runtime reload unavailable")
	}
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return errors.New("another runtime change is in progress")
	}
	m.busy = true
	m.state = StateHandover
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
	binary, err := os.Executable()
	if err == nil {
		err = handover(binary, m.opts.Args, m.opts.Listener, m.opts.AdminListener, readyTimeout, func() error { return ctx.Err() })
	}
	if err != nil {
		m.fail(err)
		return err
	}
	m.mu.Lock()
	m.state = StateIdle
	m.lastError = ""
	m.mu.Unlock()
	if m.opts.OnSwitched != nil {
		m.opts.OnSwitched()
	}
	return nil
}

// writeCurrentPointer 记录当前生效的二进制路径（原子替换）。
func writeCurrentPointer(dir, binaryPath string) error {
	pointer := filepath.Join(dir, "current")
	file, err := os.CreateTemp(dir, ".current-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	defer file.Close()
	if _, err := file.WriteString(binaryPath + "\n"); err != nil {
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, pointer)
}

// CurrentBinary 返回 current 指针指向的二进制（不存在时返回空串）。
func CurrentBinary(dir string) string {
	// #nosec G304 -- current 指针位于更新目录，非请求输入
	raw, err := os.ReadFile(filepath.Join(dir, "current"))
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(raw))
	if path == "" {
		return ""
	}
	// #nosec G703 -- 同上：current 指针内容由本进程写入
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

func (m *Manager) statusLocked() Status {
	available, reason := m.availabilityLocked()
	return Status{
		Enabled:           m.opts.Enabled,
		Visibility:        m.visibility,
		UnavailableReason: reason,
		Current:           version.Version,
		Commit:            version.Commit,
		BuiltAt:           version.BuiltAt,
		Repo:              m.client.Repo,
		Dir:               m.opts.Dir,
		State:             m.state,
		LatestTag:         m.latest.Tag,
		LatestAt:          m.latest.PublishedAt,
		UpdateReady:       available && m.latest.Tag != "" && m.latest.UpdateAvailable(),
		AssetName:         m.latest.AssetName,
		AssetSize:         m.latest.AssetSize,
		CheckedAt:         m.checkedAt,
		LastError:         m.lastError,
		InheritedFD:       Inherited(),
	}
}

// fail 记录一次失败。
//
// checkedAt **不在这里写**：它表示「远端版本已成功读取」，面板据此判断
// 「已是最新版本」并禁用「立即更新」按钮。此前 fail 也盖这个时间戳，于是一次
// 瞬时 GitHub 故障（5xx/429/网络）之后，面板会显示「已是最新版本（当前 vX，远端 ）」
// ——没有任何远端版本被读到，这句话是假的，而且按钮被禁用、点不动，直到进程重启
// 或下一次检查成功（2026-10-02 第二轮体检发现）。
// 失败状态由 state=StateFailed 与 lastError 表达，面板照此显示错误。
func (m *Manager) fail(err error) {
	m.mu.Lock()
	m.state = StateFailed
	m.lastError = err.Error()
	m.mu.Unlock()
	log.Printf("ERROR: [update] %v", err)
}
