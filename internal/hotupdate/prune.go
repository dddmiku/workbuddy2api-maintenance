// ═══ 更新日志 ═══
// 2026-09-26：更新成功后清理更新目录：保留当前与上一个运行目录，删除更早的运行目录与压缩包。
package hotupdate

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pruneKeptRuntimes 保留的运行目录数量：当前 + 上一个（回滚留一手）。
const pruneKeptRuntimes = 2

// pruneUpdateDir 清理更新目录，避免每次升级都留下一份运行目录与压缩包。
//
// 只删自己生成的东西：`runtime-*` 运行目录（保留最新两个）、下载的
// `*-wb2api-linux-*` 二进制与 `*.tar.gz` 压缩包、`.download-*.part` 残留。
// `current` 指针指向的文件及其所在目录永不删除；其余文件一律不动。
func pruneUpdateDir(dir, current string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	protected := ""
	if current != "" {
		protected = filepath.Clean(current)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type entry struct {
		path    string
		modTime int64
	}
	runtimes := []entry{}
	for _, item := range entries {
		name := item.Name()
		path := filepath.Join(dir, name)
		if item.IsDir() {
			if strings.HasPrefix(name, "runtime-") && path != protected {
				info, err := item.Info()
				if err != nil {
					continue
				}
				runtimes = append(runtimes, entry{path: path, modTime: info.ModTime().UnixNano()})
			}
			continue
		}
		if !item.Type().IsRegular() || path == protected {
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasPrefix(name, ".download-") ||
			strings.Contains(name, "-wb2api-linux-") || strings.Contains(name, "-wb2api-runtime-linux-") {
			removeAndLog(path)
		}
	}
	if len(runtimes) <= pruneKeptRuntimes-1 {
		return
	}
	sort.Slice(runtimes, func(i, j int) bool { return runtimes[i].modTime > runtimes[j].modTime })
	kept := map[string]bool{}
	if protected != "" {
		kept[filepath.Dir(protected)] = true
	}
	keepCount := pruneKeptRuntimes
	for _, item := range runtimes {
		if kept[item.path] {
			keepCount--
		}
	}
	for _, item := range runtimes {
		if kept[item.path] {
			continue
		}
		if keepCount > 0 {
			kept[item.path] = true
			keepCount--
			continue
		}
		removeAllAndLog(item.path)
	}
}

func removeAndLog(path string) {
	if err := os.Remove(path); err == nil {
		log.Printf("[update] removed %s", path)
	}
}

func removeAllAndLog(path string) {
	if err := os.RemoveAll(path); err == nil {
		log.Printf("[update] removed %s", path)
	}
}
