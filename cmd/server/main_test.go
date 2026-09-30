// ═══ 更新日志 ═══
// 2026-09-30：锁定「配置文件不存在 → 回落纯默认 + env」的兜底分支真的可达（审查发现 20）。
package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadMissingConfigIsNotExist 锁定 Load 的「文件不存在」错误可被 errors.Is 识别。
//
// 回归点：main.go 的兜底分支曾用 os.IsNotExist(err) 判断，而 Load 返回的是
// fmt.Errorf("read config: %w", err) 包装过的错误——os.IsNotExist 不拆包，对包装后的
// fs.ErrNotExist 恒为 false，于是「无 config.json、只用 WB2A_* 环境变量」的部署会在
// 启动时 log.Fatalf 而不是走默认值分支。测试同时断言 os.IsNotExist 确实失败，
// 防止有人把判断改回不拆包的旧写法。
func TestLoadMissingConfigIsNotExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-missing-config.json")
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture precondition: %v", err)
	}

	_, err := Load(missing)
	if err == nil {
		t.Fatal("Load(missing) returned nil error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("errors.Is(err, os.ErrNotExist) = false for %v; the fallback branch is unreachable", err)
	}
	// 旧写法必须失败——否则这个测试就锁定不了回归。
	if os.IsNotExist(err) {
		t.Fatalf("os.IsNotExist(err) = true for %v; the wrapped error is no longer wrapped?", err)
	}
}

// TestLoadConfigOrEnvFallsBackOnMissingFile 直接驱动启动兜底本身：文件不存在时必须
// 回落 Load("")（纯默认 + env），而不是把错误抛出去让调用方 log.Fatalf。
// 若有人把 errors.Is 改回 os.IsNotExist，这个测试会立刻变红。
func TestLoadConfigOrEnvFallsBackOnMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-missing-config.json")
	// 清空 WB2A_LISTEN，避免宿主机环境变量让默认值断言随环境漂移（applyEnv 忽略空串）。
	t.Setenv("WB2A_LISTEN", "")

	cfg, err := loadConfigOrEnv(missing)
	if err != nil {
		t.Fatalf("loadConfigOrEnv(missing) = %v; want fallback to defaults+env", err)
	}
	if cfg.Listen != ":7863" {
		t.Fatalf("fallback listen=%q want :7863", cfg.Listen)
	}
}

// TestLoadConfigOrEnvDoesNotSwallowOtherErrors 确认兜底只对「文件不存在」生效：
// 文件存在但读不了（这里是路径为目录）必须原样报错，不能静默回落默认值。
func TestLoadConfigOrEnvDoesNotSwallowOtherErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := loadConfigOrEnv(dir)
	if err == nil {
		t.Fatal("loadConfigOrEnv(dir) returned nil error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadConfigOrEnv(%q) reported not-exist for an existing directory: %v", dir, err)
	}
}
