// ═══ 更新日志 ═══
// 2026-10-13：初版——面板前端资源的内嵌集合（Go 面板用，不含 Python 文件）。
//
// 本文件放在 `panel/` 目录**里面**，因为 go:embed 只能嵌入本包目录及其子目录：
// 从 internal/panel 去嵌 ../../panel/index.html 是编译期错误。放在这里也顺带把
// 「Go 面板需要的资源」与 assets.go 的「Python 运行时需要的资源」分开。
//
// 与 assets.go 的 Files 分开是有意的：Files 里还有 app.py / key_management.py 等
// Python 文件，是给 panelruntime 解到临时目录起 Python 用的；Go 面板不需要它们，
// 把它们编进来只是白白增加体积（安卓包对体积敏感）。
//
// src/*.css 与 *.js 是构建期输入（build.py 把它们拼成 index.html），运行期不需要。
package panel

import "embed"

//go:embed index.html login.html vendor/qrcode.js
var frontendFiles embed.FS

// FrontendFiles 暴露内嵌的前端资源，供 internal/panel 的 Go 面板服务。
func FrontendFiles() embed.FS { return frontendFiles }
