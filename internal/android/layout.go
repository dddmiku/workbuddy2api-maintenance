// ═══ 更新日志 ═══
// 2026-10-13：初版——安卓端运行布局与自检（把网关+Go 面板当单二进制跑在手机上）。
//
// ═══ 为什么安卓版要自己一份布局 ═══
//
// 安卓 App 里没有「当前目录」这个概念可用：进程的 cwd 是 `/`，而应用能写的只有
// 自己的私有目录（`/data/data/<pkg>/files`）。所以配置、账号、密钥、状态文件的位置
// 全部要由 App 显式指定，不能靠相对路径。
//
// 关键约束：**数据目录必须在应用私有区**。手机上的应用私有区默认只有本应用可读
// （`/data/data/<pkg>` 权限 0700、属主是应用自己的 uid），把账号与密钥放这里是唯一
// 安全的落点；放外部存储（`/sdcard`）任何 App 都能读——那里面是能直接调上游的凭据。
package android

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layout 是安卓端的运行目录布局。
//
// 全部落在 App 私有目录下（由调用方传入，通常是 filesDir）：
//
//	<root>/config/config.json
//	<root>/auths/            <root>/auths-trash/
//	<root>/data/             <root>/panel-data/
type Layout struct {
	Root       string
	ConfigDir  string
	Config     string
	Auths      string
	AuthTrash  string
	DataDir    string
	PanelData  string
	RuntimeDir string
	LogDir     string
}

// NewLayout 按数据根目录拼出布局。
//
// RuntimeDir 与 Root 分开：辅助程序（login / credit / fission_bin）在安卓上从 APK 的
// native lib 目录解出来（见 ExtractHelpers），与用户数据不是同一个位置——用户数据要
// 能备份/清空，而辅助程序是包的一部分，清掉就该重装。
func NewLayout(root, runtimeDir string) Layout {
	return Layout{
		Root:       root,
		ConfigDir:  filepath.Join(root, "config"),
		Config:     filepath.Join(root, "config", "config.json"),
		Auths:      filepath.Join(root, "auths"),
		AuthTrash:  filepath.Join(root, "auths-trash"),
		DataDir:    filepath.Join(root, "data"),
		PanelData:  filepath.Join(root, "panel-data"),
		RuntimeDir: runtimeDir,
		LogDir:     filepath.Join(root, "logs"),
	}
}

// Ensure 建出全部目录（幂等，已存在的绝不改动内容）。
func (l Layout) Ensure() error {
	for _, dir := range []string{l.ConfigDir, l.Auths, l.AuthTrash, l.DataDir, l.PanelData, l.LogDir} {
		// 0700：账号与密钥都在这些目录里，不能给同设备上的其它 App 任何读的机会。
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
	}
	return nil
}

// DefaultConfig 生成一份开箱可用的安卓配置。
//
// 与桌面版的差别有两处，都是刻意的：
//
//  1. **监听 127.0.0.1 而不是 0.0.0.0**。手机常连公共 Wi-Fi，监听所有网卡意味着
//     同一个网络里的人可以直接调用你的账号池（而且 api_key 默认是空的）。面板与
//     API 都只服务本机——手机上的客户端 App 填 `http://127.0.0.1:7863/v1` 即可。
//
//  2. **脚本类任务默认关闭**（school / cat / redeem / lottery / makeup）。它们跑的是
//     `scripts/*.py`，而 APK 里既没有那些脚本也没有 Python——开着只会在每个时点
//     各报一次 WARN，用户看到的是「定时任务一直失败」。Go 实现的任务（签到 / 旅行 /
//     活跃上报 / 保活 / Trae 签到）不受影响，照常开启。
//
//  3. **自更新默认开启**（与桌面版相反）。桌面版是便携包，用户换一个文件夹就升级了；
//     手机用户拿不到 APK 之外的东西，自更新是它唯一能自己升级的路。
func DefaultConfig() string {
	return `{
  "listen": "127.0.0.1:7863",
  "api_key": "",
  "api_keys_file": "./data/api_keys.json",
  "api_keys_socket": "./data/api_keys.sock",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "server": {
    "max_body_mb": 8
  },
  "update": {
    "enabled": true
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "travel_hours": [9, 21],
    "activity_hours": [10],
    "keepalive_hours": [22],
    "trae_check_hours": [10],
    "checkin_enabled": true,
    "travel_enabled": true,
    "activity_enabled": true,
    "keepalive_enabled": true,
    "trae_check_enabled": true,
    "school_enabled": false,
    "cat_enabled": false,
    "redeem_enabled": false,
    "lottery_enabled": false,
    "makeup_enabled": false
  }
}
`
}

// UpdateDir 是热更新的下载目录：**<Root>/updates**。
//
// 注意是 Root 下、不是 DataDir（<Root>/data）下——App 侧 AppPaths.dataRoot()
// 返回的正是 <files>/data，也就是本结构体的 Root。两侧必须指向同一个位置，否则
// 网关下载到 A、App 去 B 找安装包，症状是「下载与验签都成功、state 回到 idle，
// 而安装器始终没被拉起」（2026-10-13 真机实测踩到两次）。
//
// 这个约定在 cmd/server/config.go 的 updateDir 里也有一份（从 WB2API_ADMIN_DIR
// 反推），改这里要一起改。
func (l Layout) UpdateDir() string {
	return filepath.Join(l.Root, "updates")
}

// UpdatedGateway 返回热更新下来的网关二进制路径；没有则返回空串。
//
// ═══ 为什么冷启动必须优先用它 ═══
//
// 安卓的热更新替换的是**可执行文件**（网关以文件形式跑在应用私有目录里），
// 而 APK 的 nativeLibraryDir 是只读的、改不了。所以更新后的二进制落在数据目录，
// 由这里在每次启动时优先选中。
//
// 不做这一步，热更新就是个假的：新版本会被下载、校验、交接成功，进程内一路正常
// ——直到用户把 App 划掉再打开，又从 nativeLibraryDir 拉起 APK 里的旧二进制，
// 版本悄悄回退。用户看到的是「更新了但重启又变回旧版」。
//
// 桌面版同理（见 cmd/desktop/main.go），两边用同一个 current 指针格式。
func (l Layout) UpdatedGateway() string {
	dir := l.UpdateDir()
	raw, err := os.ReadFile(filepath.Join(dir, "current"))
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(raw))
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

// EnsureConfig 首次运行时写入默认配置；已存在的一律不动。
//
// 绝不覆盖是硬要求：用户改过的配置（换端口、加密钥、调排程）不能在下次启动时
// 被重置回默认——「重启一次设置就没了」会让人再也不敢改配置。
func (l Layout) EnsureConfig() (bool, error) {
	if _, err := os.Stat(l.Config); err == nil {
		return false, nil
	}
	if err := os.WriteFile(l.Config, []byte(DefaultConfig()), 0600); err != nil {
		return false, fmt.Errorf("写入默认配置: %w", err)
	}
	return true, nil
}
