// ═══ 更新日志 ═══
// 2026-09-25：管理台随Go主程序嵌入与发布，保证热更新后前后端使用同一版本。
package panel

import "embed"

// Files includes only application assets; credentials and local test data can
// never become part of a binary even when they are next to the sources.
//
//go:embed app.py key_management.py native_runtime.py file_lock.py index.html login.html vendor/qrcode.js
var Files embed.FS
