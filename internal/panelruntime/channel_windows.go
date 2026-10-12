//go:build windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——Windows 用回环 TCP 作为面板通信通道（Python 标准库没有 UnixStreamServer）。
package panelruntime

import (
	"net"
	"net/http"
	"time"
)

// channel 返回面板通信通道：**仅回环的 TCP**。
//
// 为什么 Windows 不能用 Unix socket：Python 标准库的 socketserver 在 Windows 上
// 没有 UnixStreamServer（AF_UNIX 内核支持，但标准库不封装），面板会在启动处
// AttributeError 崩溃（2026-10-13 桌面打包实测）。
//
// 安全边界：绑定 127.0.0.1 的**随机空闲端口**，不是 0.0.0.0。面板里带账号与
// 密钥，绝不能暴露到局域网；随机端口同时避免与用户其它程序抢端口。
func (r *Runtime) channel() (*http.Transport, map[string]string) {
	addr := r.loopbackAddr()
	transport := &http.Transport{
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 4 * time.Minute,
	}
	return transport, map[string]string{
		// 面板侧据此绑 TCP；两个变量互斥，面板优先看 SOCKET（未设则走 TCP）。
		"WB2API_PANEL_SOCKET": "",
		"WB2API_PANEL_TCP":    addr,
	}
}

// proxyHost 代理目标 host：TCP 通道下必须是真实地址，写 localhost 会去连 80 端口。
func (r *Runtime) proxyHost() string { return r.socket }

// loopbackAddr 选一个空闲的回环端口并记在 r.socket 上（供代理与就绪探测复用）。
//
// 端口先绑再放：这样拿到的是「此刻确实空闲」的端口。存在极小概率被别的进程
// 抢走，但面板启动失败会由既有的 RetryInitialFailure 重试路径覆盖。
func (r *Runtime) loopbackAddr() string {
	if r.socket != "" && r.socket != "panel.sock" && !isUnixPath(r.socket) {
		return r.socket
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.socket = "127.0.0.1:17864"
		return r.socket
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r.socket = addr
	return addr
}

// isUnixPath 判断 socket 字段里存的还是不是 Unix 路径。
//
// Start() 初始化时把 r.socket 设成了 Unix socket 路径；Windows 上这个字段
// 改存 TCP 地址，所以要能区分「尚未替换」与「已是地址」。
func isUnixPath(value string) bool {
	for _, ch := range value {
		if ch == '/' || ch == '\\' {
			return true
		}
	}
	return false
}
