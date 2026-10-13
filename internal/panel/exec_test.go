// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住原生模式下的日志读取（桌面/安卓的面板日志页靠它）。
package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTailLogFileReadsRotationOrder 原生模式下日志页必须能读到内容。
//
// 真机事故（2026-10-13）：NativeRunner 的 logs 分支原本只返回「请直接查看运行日志
// 文件」，于是**桌面与安卓上面板的日志页是空白的**（/api/logs 返回 rc=1、rows=[]）。
// Python 版的 native_runtime 实现了同一件事，Go 版漏了，同一个面板在两个引擎下表现
// 不一致。
//
// 轮转顺序也一并锁住：runlog 把当前文件改名成 .1、旧 .1 变成 .2，所以时间上最早的
// 是 .2。顺序反了会让「最近的日志」里混进更早的内容——而用户看日志恰恰是为了找
// 「刚刚发生了什么」。
func TestTailLogFileReadsRotationOrder(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "gateway.log")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(base+".2", "oldest-1\noldest-2\n")
	write(base+".1", "middle-1\n")
	write(base, "newest-1\nnewest-2\n")

	result := tailLogFile(base, "10")
	if result.Code != 0 {
		t.Fatalf("应成功，实际 code=%d stderr=%s", result.Code, result.Stderr)
	}
	want := "oldest-1\noldest-2\nmiddle-1\nnewest-1\nnewest-2\n"
	if result.Stdout != want {
		t.Fatalf("日志顺序不对：\n得到 %q\n期望 %q", result.Stdout, want)
	}
}

// TestTailLogFileHonorsCount 只要最后 N 行。
func TestTailLogFileHonorsCount(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "gateway.log")
	if err := os.WriteFile(base, []byte("a\nb\nc\nd\ne\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tailLogFile(base, "2").Stdout; got != "d\ne\n" {
		t.Fatalf("取最后 2 行，得到 %q", got)
	}
}

// TestTailLogFileMissingFileIsNotAnError 日志文件还没生成时不该报错。
//
// 首次启动、或刚轮转过，某些档位就是不存在——那是正常情况，报错会让面板日志页
// 显示一个「读取失败」而其实只是还没写。
func TestTailLogFileMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	result := tailLogFile(filepath.Join(dir, "nonexistent.log"), "5")
	if result.Code != 0 {
		t.Fatalf("文件不存在不该算失败，实际 code=%d stderr=%s", result.Code, result.Stderr)
	}
	if result.Stdout != "" {
		t.Fatalf("没有日志时应返回空，实际 %q", result.Stdout)
	}
}

// TestTailLogFileRejectsBadCount 非数字的条数要明确拒绝，而不是静默给 0 行。
func TestTailLogFileRejectsBadCount(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "gateway.log")
	if err := os.WriteFile(base, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := tailLogFile(base, "abc"); result.Code == 0 {
		t.Fatal("非数字条数应报错")
	}
	if !strings.Contains(tailLogFile(base, "abc").Stderr, "无效") {
		t.Fatal("错误信息应说明条数无效")
	}
}

// TestTailLogFileSkipsPartialFirstLine 超大文件只读尾部时，第一行可能是半截的，
// 必须丢掉——否则日志页会显示一行残缺的乱码。
func TestTailLogFileSkipsPartialFirstLine(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "gateway.log")
	// 造一个超过上限的文件，让读取从中间开始。
	limit := int(logTailLimit)
	content := strings.Repeat("X", limit) + "\ncomplete-line\n"
	if err := os.WriteFile(base, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailLogFile(base, "10").Stdout
	if strings.Contains(got, "XXXX") {
		t.Fatalf("截断处的半截行必须丢掉，实际 %q", got)
	}
	if !strings.Contains(got, "complete-line") {
		t.Fatalf("完整的行必须保留，实际 %q", got)
	}
}

// TestResolveHelperFallsBackToGatewayDir 辅助程序不在网关旁边时，要回落到工作目录。
//
// 容器形态就是这样：部署脚本把网关本体放在 data/updates/runtime-*/ 下（热更新换的
// 就是它），而辅助程序留在 /app。只看网关旁边会让「Go 引擎 + 容器形态」下扫码加号、
// 积分查询、补发邀请全部报「运行辅助程序不可用」——2026-10-13 在 VPS 上隔离验证时
// 发现（当时报的是启动期的 "incomplete runtime: login is unavailable"）。
func TestResolveHelperFallsBackToGatewayDir(t *testing.T) {
	binDir := t.TempDir()  // 模拟 data/updates/runtime-*/（只有网关本体）
	workDir := t.TempDir() // 模拟 /app（辅助程序在这里）
	helper := filepath.Join(workDir, platformExecutableName("login"))
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &NativeRunner{RuntimeDir: binDir, GatewayDir: workDir}
	if got := runner.resolveHelper("login"); got != helper {
		t.Fatalf("应回落到工作目录的 %q，实际 %q", helper, got)
	}
}

// TestResolveHelperPrefersRuntimeDir 两边都有时，网关旁边的优先（桌面/安卓形态）。
func TestResolveHelperPrefersRuntimeDir(t *testing.T) {
	binDir := t.TempDir()
	workDir := t.TempDir()
	for _, dir := range []string{binDir, workDir} {
		if err := os.WriteFile(filepath.Join(dir, platformExecutableName("login")), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runner := &NativeRunner{RuntimeDir: binDir, GatewayDir: workDir}
	if got := runner.resolveHelper("login"); got != filepath.Join(binDir, platformExecutableName("login")) {
		t.Fatalf("网关旁边有就不该回落，实际 %q", got)
	}
}

// TestResolveHelperHonorsExplicitPath 显式指定的路径（安卓的 nativeLibraryDir）最优先。
func TestResolveHelperHonorsExplicitPath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "libwb2login.so")
	runner := &NativeRunner{RuntimeDir: t.TempDir(), GatewayDir: t.TempDir(),
		HelperPaths: map[string]string{"login": explicit}}
	if got := runner.resolveHelper("login"); got != explicit {
		t.Fatalf("显式路径应最优先，实际 %q", got)
	}
}
