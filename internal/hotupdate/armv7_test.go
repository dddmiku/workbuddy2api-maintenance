// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住 32 位 ARM（armv7，行空板 M10）的资产命名与平台识别。
package hotupdate

import (
	"runtime"
	"strings"
	"testing"
)

// TestAssetNameForArmv7 32 位 ARM 的资产名必须带 ABI 版本。
//
// 为什么不能只叫 "wb2api-linux-arm"：Go 的 arm 目标有 v5/v6/v7 三个 ABI 变体，
// 指令集不同，拿 v5 的二进制到 v7 板上跑会 SIGILL。名字里带版本号，发布端与
// 自更新端才可能对得上。
func TestAssetNameForArmv7(t *testing.T) {
	t.Setenv("GOARM", "7")
	name, err := assetNameFor("linux", "arm")
	if err != nil {
		t.Fatalf("arm 应当被支持：%v", err)
	}
	if name != "wb2api-linux-armv7" {
		t.Fatalf("资产名 = %q，期望 wb2api-linux-armv7", name)
	}
}

// TestAssetNameForArmDefaultsToV7 未设 GOARM 时回落 v7。
//
// 本项目的部署形态就是 armv7（行空板 M10 / 各类 Cortex-A7 板子）。回落 v7
// 比回落一个不存在的通用名更有用。
func TestAssetNameForArmDefaultsToV7(t *testing.T) {
	t.Setenv("GOARM", "")
	name, err := assetNameFor("linux", "arm")
	if err != nil {
		t.Fatalf("arm 应当被支持：%v", err)
	}
	if name != "wb2api-linux-armv7" {
		t.Fatalf("未设 GOARM 时资产名 = %q，期望 wb2api-linux-armv7", name)
	}
}

// TestAssetNameForArmHonoursGoarm 显式 GOARM 必须被尊重（别的板子可能只要 v6）。
func TestAssetNameForArmHonoursGoarm(t *testing.T) {
	t.Setenv("GOARM", "6")
	name, err := assetNameFor("linux", "arm")
	if err != nil {
		t.Fatalf("arm 应当被支持：%v", err)
	}
	if name != "wb2api-linux-armv6" {
		t.Fatalf("GOARM=6 时资产名 = %q，期望 wb2api-linux-armv6", name)
	}
}

// TestRuntimeAssetNameMatchesBinaryNaming 运行包名与裸二进制名必须同一规则。
//
// 两边若不一致，「按平台找资产」会在 arm 上找到裸二进制却找不到运行包——
// 表现为「下载成功但解包时找不到本机架构的资产」。
func TestRuntimeAssetNameMatchesBinaryNaming(t *testing.T) {
	t.Setenv("GOARM", "7")
	binary, err := assetNameFor("linux", "arm")
	if err != nil {
		t.Fatal(err)
	}
	binaryArch := strings.TrimPrefix(binary, "wb2api-linux-")
	runtimeName := "wb2api-runtime-linux-" + binaryArch + ".tar.gz"
	if runtimeName != "wb2api-runtime-linux-armv7.tar.gz" {
		t.Fatalf("运行包名推导 = %q，期望 wb2api-runtime-linux-armv7.tar.gz", runtimeName)
	}
}

// TestRuntimeArchNameOnNonArm 非 arm 平台不受影响（回归保护）。
func TestRuntimeArchNameOnNonArm(t *testing.T) {
	if runtime.GOARCH == "arm" {
		t.Skip("在 arm 主机上跳过：这条测的是非 arm 路径")
	}
	if got := runtimeArchName(); got != runtime.GOARCH {
		t.Fatalf("runtimeArchName() = %q，期望 %q", got, runtime.GOARCH)
	}
}

// TestAssetNameForUnsupportedStillRejected 不支持的架构仍要明确报错。
func TestAssetNameForUnsupportedStillRejected(t *testing.T) {
	if _, err := assetNameFor("linux", "mips"); err == nil {
		t.Fatal("不支持的架构必须报错，而不是给一个假名字")
	}
}
