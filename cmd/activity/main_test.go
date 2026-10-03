package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/config"
)

// TestNewUpstreamGlobalEnabled registry：cmd/activity 与 cmd/signin、cmd/trial、cmd/credit
// 同风格显式接线 GlobalEnabled=true。activity 虽是 CN 任务中心的上报工具（global 账号已被
// scheduler 的 IsGlobal 门控跳过），但工具自身构造 global 账号请求时不得误打 CN base——
// 开关显式开启把该路径收敛为「不正确但不会跨域误打」（与 signin/trial 同逃生门式接线）。
func TestNewUpstreamGlobalEnabled(t *testing.T) {
	up := newUpstream(&cfgFile{Schedule: config.DefaultSchedule()})
	if !up.GlobalEnabled {
		t.Error("newUpstream should set GlobalEnabled=true (explicit wiring, uniform with signin/trial/credit)")
	}
}

// TestNewUpstreamAppliesTimeout 配置的 upstream.timeout_seconds 仍生效（接线抽出不破坏原行为）。
func TestNewUpstreamAppliesTimeout(t *testing.T) {
	c := &cfgFile{Schedule: config.DefaultSchedule()}
	c.Upstream.TimeoutSeconds = 7
	up := newUpstream(c)
	if up.HTTP.Timeout == 0 {
		t.Fatal("HTTP.Timeout should be set when timeout_seconds > 0")
	}
	if up.HTTP.Timeout.String() != "7s" {
		t.Errorf("HTTP.Timeout=%v want 7s", up.HTTP.Timeout)
	}
}

// TestConfigCandidates 探测顺序：显式 > WB2A_CONFIG > config/config.json > ./config.json。
func TestConfigCandidates(t *testing.T) {
	t.Setenv("WB2A_CONFIG", "")
	if got := configCandidates("/x/custom.json"); len(got) != 1 || got[0] != "/x/custom.json" {
		t.Fatalf("explicit: %v", got)
	}
	t.Setenv("WB2A_CONFIG", "/env/cfg.json")
	if got := configCandidates(""); len(got) != 1 || got[0] != "/env/cfg.json" {
		t.Fatalf("env: %v", got)
	}
	t.Setenv("WB2A_CONFIG", "")
	got := configCandidates("")
	want := []string{filepath.Join("config", "config.json"), "config.json"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("default candidates=%v want %v", got, want)
	}
}

// TestLoadConfigFileFindsUnifiedLayout 回归：统一运行包把配置放 config/config.json。
//
// 回归点：cmd/activity 此前硬编码 os.ReadFile("config.json")，容器内 -w /app 的文档用法
// 打开 /app/config.json 必然失败（compose 只挂 /app/config）。现在 cwd 下探测
// config/config.json，找不到才回落 ./config.json，都没有时返回 nil 走默认+env。
func TestLoadConfigFileFindsUnifiedLayout(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("WB2A_CONFIG", "")
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	unified := filepath.Join(dir, "config", "config.json")
	if err := os.WriteFile(unified, []byte(`{"auth_dir":"./auths"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, path, err := loadConfigFile("")
	if err != nil {
		t.Fatalf("loadConfigFile: %v", err)
	}
	if raw == nil || !strings.HasSuffix(path, filepath.Join("config", "config.json")) {
		t.Fatalf("unified layout not found: path=%q", path)
	}

	// 移走统一布局后回落到项目根的 ./config.json。
	if err := os.Remove(unified); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "config.json")
	if err := os.WriteFile(legacy, []byte(`{"auth_dir":"./legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, path, err = loadConfigFile("")
	if err != nil || raw == nil || path != "config.json" {
		t.Fatalf("legacy fallback: raw=%v path=%q err=%v", raw, path, err)
	}

	// 两个都没有：返回 nil 交给「默认 + env」，不再 log.Fatalf。
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	raw, path, err = loadConfigFile("")
	if err != nil || raw != nil || path != "" {
		t.Fatalf("missing both: raw=%v path=%q err=%v want (nil,\"\",nil)", raw, path, err)
	}
}

// TestLoadConfigFilePropagatesRealErrors 非「文件不存在」的错误必须原样返回（不能兜底吞掉）。
func TestLoadConfigFilePropagatesRealErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := loadConfigFile(dir); err == nil {
		t.Fatal("目录当配置文件读取应当报错，而不是被当成「不存在」兜底")
	}
}
