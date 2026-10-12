//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Linux/macOS 用 Unix socket 作为面板通信通道（与既有行为逐字一致）。
package panelruntime

import (
	"context"
	"net"
	"net/http"
	"time"
)

// channel 返回面板通信通道：Unix domain socket。
//
// 这是容器/服务器上的既有形态，行为零变更。socket 文件放在面板的临时目录里
// （0700），只有本机同用户可达；网关用 unix dial 连它。
func (r *Runtime) channel() (*http.Transport, map[string]string) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
		},
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 4 * time.Minute,
	}
	return transport, map[string]string{"WB2API_PANEL_SOCKET": r.socket}
}

// proxyHost 代理目标 host。unix 通道下 DialContext 忽略它，写 localhost 即可。
func (r *Runtime) proxyHost() string { return "localhost" }
