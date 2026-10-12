// ═══ 更新日志 ═══
// 2026-09-18：锁定监督脚本与 Go 共用的更新目录，保留文件配置和状态目录的回退语义。
package main

import (
	"path/filepath"
	"testing"
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
