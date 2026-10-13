// ═══ 更新日志 ═══
// 2026-09-18：锁定监督脚本与 Go 共用的更新目录，保留文件配置和状态目录的回退语义。
package main

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/panel"
)

func TestUpdateDirUsesSupervisorDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "supervisor-updates")
	t.Setenv("WB2API_UPDATE_DIR", dir)
	config := Default()
	config.Update.Dir = filepath.Join(t.TempDir(), "file-config-updates")
	if got := updateDir(config); got != dir {
		t.Fatalf("update directory = %q; supervisor restarts from %q", got, dir)
	}
}

func TestUpdateDirKeepsFileFallbacks(t *testing.T) {
	t.Setenv("WB2API_UPDATE_DIR", "")
	config := Default()
	custom := filepath.Join(t.TempDir(), "custom")
	config.Update.Dir = custom
	if got := updateDir(config); got != custom {
		t.Fatalf("explicit update directory = %q want %q", got, custom)
	}
	config.Update.Dir = ""
	config.StateFile = filepath.Join(t.TempDir(), "state.json")
	if got, want := updateDir(config), filepath.Join(filepath.Dir(config.StateFile), "updates"); got != want {
		t.Fatalf("state-derived update directory = %q want %q", got, want)
	}
	// state_file 是相对路径时必须**绝对化**。
	//
	// 安卓上进程的 cwd 是 "/"，应用无权在根目录建目录；桌面版的数据根由启动器
	// 通过 WB2A_DESKTOP_ROOT 指定。返回相对路径会让「下载到 A、App 去 B 找安装包」
	// ——实测症状是下载与验签都成功、state 回到 idle，而安装器始终没被拉起
	// （标记文件根本没写到 App 看的地方）。
	config.StateFile = "state.json"
	got := updateDir(config)
	if !filepath.IsAbs(got) {
		t.Fatalf("更新目录必须是绝对路径（安卓 cwd 是 /），实际 %q", got)
	}
	if filepath.Base(got) != "updates" {
		t.Fatalf("更新目录应以 updates 结尾，实际 %q", got)
	}
}

// TestUpdateDirExplicitBeatsConfig 启动方显式指定的更新目录必须压过配置里的
// update.dir。
//
// 为什么这条重要：启动器（桌面版 cmd/desktop）与 App（安卓 GatewayService）冷
// 启动时看的都是**它们自己算出来的那个目录**，而它们都会把 WB2API_UPDATE_DIR
// 传给网关。如果网关让配置里的 update.dir 压过它，用户一改配置两边就分叉——
// 网关把更新下到 A、启动器去 B 找，症状是「更新成功但重启还是旧版」或「下载与
// 验签都成功、安装器始终不被拉起」。这两条都在 2026-10-13 真机上实际踩到过。
func TestUpdateDirExplicitBeatsConfig(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "launcher-updates")
	t.Setenv("WB2API_UPDATE_DIR", explicit)
	config := Default()
	config.Update.Dir = filepath.Join(t.TempDir(), "from-config")
	config.StateFile = filepath.Join(t.TempDir(), "state.json")
	if got := updateDir(config); got != explicit {
		t.Fatalf("updateDir() = %q，期望显式的 %q（配置里的 update.dir 不应压过它）", got, explicit)
	}
}

// TestUpdateDirAndroidAgreesWithApp 安卓侧「网关写哪里」必须等于「App 看哪里」。
//
// App（GatewayService.startGateway）设 WB2API_ADMIN_DIR=<root>/panel-data 与
// WB2API_UPDATE_DIR=<root>/updates，而它的 watcher 盯的正是 <root>/updates。
// 这里锁住「即使 App 不显式传 WB2API_UPDATE_DIR，网关从 WB2API_ADMIN_DIR 反推
// 出来的也必须是同一个目录」——这是 App 侧改动前的兼容路径，也是文档里写下的
// 那份约定。前两次分叉（v2.15.3、v2.15.4）都是因为这条不成立而没人测。
func TestUpdateDirAndroidAgreesWithApp(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WB2API_UPDATE_DIR", "")
	t.Setenv("WB2API_ANDROID", "1")
	t.Setenv("WB2API_ADMIN_DIR", filepath.Join(root, "panel-data"))
	config := Default()
	config.Update.Dir = ""
	config.StateFile = filepath.Join(root, "data", "state.json")
	want := filepath.Join(root, "updates")
	if got := updateDir(config); got != want {
		t.Fatalf("安卓反推的更新目录 = %q，期望 %q（App 的 watcher 盯的就是这个）", got, want)
	}
}

// TestSessionTTLForHost 只有安卓拿长期会话，桌面与容器保持默认 30 天。
//
// 安卓：面板是 App 里 WebView 长期挂着的界面，手机已有锁屏与生物识别，
// 每次打开都重输密码是纯粹的折磨（用户 2026-10-13 明确要求设为永久）。
// 桌面/容器：面对的是浏览器，会话有效期是那里唯一的一道门。
func TestSessionTTLForHost(t *testing.T) {
	t.Setenv("WB2API_ANDROID", "")
	if got := sessionTTLForHost(); got != 0 {
		t.Fatalf("桌面/容器应保持默认 TTL（返回 0 让面板用 30 天），实际 %v", got)
	}
	t.Setenv("WB2API_ANDROID", "1")
	if got := sessionTTLForHost(); got != panel.LongSessionTTL {
		t.Fatalf("安卓应拿到长期 TTL %v，实际 %v", panel.LongSessionTTL, got)
	}
}
