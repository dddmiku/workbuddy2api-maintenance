// ═══ 更新日志 ═══
// 2026-10-12：初版——锁住「辅助二进制清单」的四处同步。
//
// 加一个辅助程序（如 fission_bin / fission.sh）必须同时改四处：
//
//	internal/hotupdate/bundle.go       runtimeFiles（解包白名单 + 权限表）
//	scripts/runtime_bundle.py          FILES（打包含清单）
//	cmd/server/panel.go                prepareUnified 的完整性校验
//	Dockerfile                         构建 + COPY
//
// 漏掉 runtime_bundle.py 会让包缺文件（bundle.go 的等长校验直接拒绝升级）；
// 漏掉 bundle.go 会让升级包被判成「含未知成员」。两者都表现为升级失败，
// 但报错位置离真正的原因很远。这里把清单本身锁住，改动漏一处就红。
//
// 同类事故有先例：2026-10-11 实测「新版网关 + 旧版辅助程序」并存，credit 的 trae
// 分支因为跑的是旧二进制而从未生效，面板上 trae 账号一直显示查询失败。
package hotupdate

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoFile 读仓库内文件（测试在 internal/hotupdate 下跑，往上两级到 gateway/）。
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("读 %s: %v", rel, err)
	}
	return string(raw)
}

// goRuntimeFiles 从 bundle.go 抽出 runtimeFiles 的键集。
func goRuntimeFiles(t *testing.T) []string {
	t.Helper()
	src := repoFile(t, "internal/hotupdate/bundle.go")
	m := regexp.MustCompile(`(?s)var runtimeFiles = map\[string\]os\.FileMode\{(.*?)\n\}`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("bundle.go 里找不到 runtimeFiles 定义")
	}
	var out []string
	for _, sub := range regexp.MustCompile(`"([^"]+)":\s*0\d+`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, sub[1])
	}
	sort.Strings(out)
	return out
}

// pythonRuntimeFiles 从 scripts/runtime_bundle.py 抽出 FILES 元组。
func pythonRuntimeFiles(t *testing.T) []string {
	t.Helper()
	src := repoFile(t, "scripts/runtime_bundle.py")
	m := regexp.MustCompile(`(?s)FILES = \((.*?)\)\n`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("runtime_bundle.py 里找不到 FILES 定义")
	}
	var out []string
	for _, sub := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, sub[1])
	}
	sort.Strings(out)
	return out
}

// TestRuntimeFileListsMatch 两份清单必须逐字相同。
//
// 这是升级包能否被接受的前提：bundle.go 用 len(runtimeFiles) 与 manifest 比对，
// 任一侧多/少一个条目都会让所有升级在解包阶段失败。
func TestRuntimeFileListsMatch(t *testing.T) {
	goList := goRuntimeFiles(t)
	pyList := pythonRuntimeFiles(t)
	if len(goList) == 0 {
		t.Fatal("bundle.go 的 runtimeFiles 解析为空——正则可能已失效")
	}
	if strings.Join(goList, "\n") != strings.Join(pyList, "\n") {
		t.Fatalf("清单不一致：\n  bundle.go (%d): %v\n  runtime_bundle.py (%d): %v",
			len(goList), goList, len(pyList), pyList)
	}
}

// TestFissionRegisteredEverywhere fission 必须在四处都登记。
//
// 只加代码不改这四处，产物是「本地能跑、升级包缺文件」或「升级包被拒」。
// 具体：漏 bundle.go → 解包报 invalid runtime member；漏 runtime_bundle.py →
// 报 missing required files；漏 Dockerfile → 容器里没有这个二进制；
// 漏 panel.go → 启动时直接报 incomplete runtime。
func TestFissionRegisteredEverywhere(t *testing.T) {
	goSrc := repoFile(t, "internal/hotupdate/bundle.go")
	if !strings.Contains(goSrc, `"fission_bin": 0755`) {
		t.Error("bundle.go 的 runtimeFiles 缺少 fission_bin")
	}
	if !strings.Contains(goSrc, `"fission.sh": 0755`) {
		t.Error("bundle.go 的 runtimeFiles 缺少 fission.sh")
	}

	if !contains(pythonRuntimeFiles(t), "fission_bin") {
		t.Error("runtime_bundle.py 的 FILES 缺少 fission_bin")
	}
	if !contains(pythonRuntimeFiles(t), "fission.sh") {
		t.Error("runtime_bundle.py 的 FILES 缺少 fission.sh")
	}

	panelSrc := repoFile(t, "cmd/server/panel.go")
	if !strings.Contains(panelSrc, `"fission_bin"`) {
		t.Error("panel.go 的完整性校验缺少 fission_bin")
	}

	docker := repoFile(t, "Dockerfile")
	for _, want := range []string{"./cmd/fission", "/out/fission_bin", "/app/fission_bin", "fission.sh"} {
		if !strings.Contains(docker, want) {
			t.Errorf("Dockerfile 缺少 %s", want)
		}
	}
}

// TestFissionWrapperScriptShipped 包装脚本必须存在且转发到 fission_bin。
//
// 脚本是运维入口（容器内 ./fission.sh）。它不在镜像里，补发就得进容器手敲
// curl，等于这个工具白做。
func TestFissionWrapperScriptShipped(t *testing.T) {
	src := repoFile(t, "fission.sh")
	if !strings.Contains(src, "fission_bin") {
		t.Fatal("fission.sh 未指向 fission_bin")
	}
	if !strings.Contains(src, "cmd/fission") {
		t.Fatal("fission.sh 缺少从源码构建的分支")
	}
	// 与其它包装脚本同构：容器内预置产物优先，其次缓存，最后源码构建。
	for _, want := range []string{"WB2A_AUTH_DIR", "command -v go"} {
		if !strings.Contains(src, want) {
			t.Errorf("fission.sh 缺少 %q", want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
