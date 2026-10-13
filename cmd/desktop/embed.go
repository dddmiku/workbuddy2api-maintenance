// ═══ 更新日志 ═══
// 2026-10-13：初版——单文件分发时把网关与辅助程序内嵌进启动器，首次运行解出来。
//
// ═══ 为什么要内嵌 ═══
//
// 目录分发对用户是负担：解压出来一堆 exe，双击哪个、能不能挪走、删哪个都靠猜。
// 用户 2026-10-13 明确要求「打包成单 exe 就行，简化下操作」。
//
// 内嵌的是**同一批二进制**：网关仍是 cmd/server 编出来的那一个，行为与容器/服务器
// 逐字一致。单文件分发只是换了个投递方式，不是第二套实现。
//
// ═══ 为什么解到数据目录而不是临时目录 ═══
//
// 解到 %TEMP% 有两个问题：一是 Temp 会被清理工具随时删掉（进程还在跑、文件没了），
// 二是本机用户的 Temp 曾因海量小文件导致登录变慢（见全局约定）。解到 <root>/runtime
// 则「程序 + 数据」在一起，用户备份仍是拷一个文件夹，卸载也只是删那一个目录。
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// embeddedRuntimeFS 是构建脚本预置到 embedded/ 下的运行时文件（网关 + 辅助程序）。
//
// 用 go:embed 而不是「生成一个装着 []byte 的 .go 文件」：后者会把十几 MB 的内容
// 变成同样大的**源码**，每次构建都要让编译器解析一遍，且 diff、grep、编辑器都会被
// 它拖垮。go:embed 把内容当数据附着，编译快、源码树里也看不到它们。
//
// 目录分发构建时 embedded/ 里只有占位文件（见 embedded/PLACEHOLDER），此时
// hasEmbeddedRuntime() 返回 false，一切走「exe 旁边」的老路径。
//
//go:embed all:embedded
var embeddedRuntimeFS embed.FS

// hasEmbeddedRuntime 本次构建是否内嵌了运行时。
func hasEmbeddedRuntime() bool {
	files, err := runtimeFiles()
	return err == nil && len(files) > 0
}

// runtimeFiles 返回内嵌清单（相对路径 → 内容）。
//
// 跳过点开头的条目：构建脚本用它们占位（go:embed 不接受空目录），它们不是运行时
// 的一部分。保留子目录结构（scripts/ 下的任务脚本要落在 <root>/runtime/scripts/）。
func runtimeFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(embeddedRuntimeFS, "embedded", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative := strings.TrimPrefix(path, "embedded/")
		if relative == "" || relative == "embedded" {
			return nil
		}
		for _, segment := range strings.Split(relative, "/") {
			if strings.HasPrefix(segment, ".") {
				// 占位文件（.PLACEHOLDER）与版本控制残留：整个子树都跳过。
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() {
			return nil
		}
		content, err := embeddedRuntimeFS.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// extractEmbeddedRuntime 把内嵌的运行时解到 dir。
//
// **幂等**：内容与已存在文件相同时不重写。重写的代价不只是启动慢——每次启动都改
// mtime 会让「按时间找最新文件」的排查手段失效，也会让杀软每次都重扫一遍。
//
// 写盘用「临时文件 + 改名」：直接写目标文件时，另一台机器上同时启动两个实例
// （或前一次被杀软中断）会留下半个文件，而那个文件看起来是存在的、跑起来才报错。
// 改名在同一文件系统内是原子的，读到的要么是旧版要么是新版，不会是半截。
func extractEmbeddedRuntime(dir string) error {
	files, err := runtimeFiles()
	if err != nil {
		return fmt.Errorf("读取内嵌运行时失败: %w", err)
	}
	if len(files) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("无法创建运行目录 %s: %w", dir, err)
	}
	for name, content := range files {
		// 路径不能由数据决定：拒绝绝对路径与 ".."，其余交给 filepath.Clean 归一。
		if name == "" || filepath.IsAbs(name) || name == ".." ||
			strings.HasPrefix(name, "../") || strings.Contains(name, `\`) {
			return fmt.Errorf("内嵌文件名非法: %q", name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("无法创建目录 %s: %w", filepath.Dir(target), err)
		}
		if sameContent(target, content) {
			continue
		}
		temp := target + ".tmp"
		if err := os.WriteFile(temp, content, 0o755); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", name, err)
		}
		// Windows 上目标可能正被上一实例占用：此时改名会失败，但那说明**有另一个
		// 实例在用同一个运行目录**，报出来比静默继续更好。
		if err := os.Rename(temp, target); err != nil {
			_ = os.Remove(temp)
			return fmt.Errorf("替换 %s 失败（可能另一个实例正在运行）: %w", name, err)
		}
	}
	return nil
}

// sameContent 目标文件是否已与内容一致（比长度再比摘要，避免读大文件后才判定不同）。
func sameContent(path string, content []byte) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() != int64(len(content)) {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return false
	}
	want := sha256.Sum256(content)
	return hex.EncodeToString(digest.Sum(nil)) == hex.EncodeToString(want[:])
}
