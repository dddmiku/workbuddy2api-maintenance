// ═══ 更新日志 ═══
// 2026-09-25：发行目标统一记录在根 VERSION；保留未注入构建的 dev 身份，避免误报正式版本。
// 2026-09-17：新增版本元数据：构建时经 ldflags 注入，供 /healthz、/update/status
//
//	与自更新比对使用。
package version

import "strings"

// 构建期注入（Dockerfile / 私有工作流的 -ldflags "-X ...=..."）。根 VERSION
// 记录源码的发行目标；未注入时仍使用开发态默认值，不据目标版本冒充已发布二进制。
var (
	// Version 是实际构建版本；本轮发行目标例如 v2.3.0。
	Version = "dev"
	// Commit 构建来源提交（短 SHA 或完整 SHA）。
	Commit = "unknown"
	// BuiltAt 构建时间（RFC3339 或任意可读字符串）。
	BuiltAt = "unknown"
)

// String 返回一行可读的版本描述。
func String() string {
	parts := []string{Version}
	if Commit != "" && Commit != "unknown" {
		commit := Commit
		if len(commit) > 8 {
			commit = commit[:8]
		}
		parts = append(parts, commit)
	}
	if BuiltAt != "" && BuiltAt != "unknown" {
		parts = append(parts, BuiltAt)
	}
	return strings.Join(parts, " ")
}

// IsDev 判断是否为未注入版本的开发构建（自更新对 dev 构建仍允许，但会提示）。
func IsDev() bool { return Version == "" || Version == "dev" }
