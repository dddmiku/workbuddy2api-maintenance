// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住下载加速代理的改写范围与关闭方式。
package hotupdate

import (
	"strings"
	"testing"
)

// TestProxiedRewritesGitHubURLs 代理只改写 GitHub 的地址。
func TestProxiedRewritesGitHubURLs(t *testing.T) {
	c := &Client{ProxyBase: "https://gh-proxy.com"}
	cases := map[string]string{
		"https://github.com/o/r/releases/download/v1/a.apk": "https://gh-proxy.com/https://github.com/o/r/releases/download/v1/a.apk",
		"https://objects.githubusercontent.com/x/y":         "https://gh-proxy.com/https://objects.githubusercontent.com/x/y",
		"https://api.github.com/repos/o/r/releases/latest":  "https://gh-proxy.com/https://api.github.com/repos/o/r/releases/latest",
	}
	for in, want := range cases {
		if got := c.proxied(in); got != want {
			t.Errorf("proxied(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestProxiedRefusesForeignHosts 非 GitHub 主机一律不改写。
//
// 这是 SSRF 纵深防御：改写会把目标地址拼进 URL，若不加限制，一个被篡改的
// browser_download_url 就能让网关去请求任意主机。资产 URL 来自 GitHub API，
// 但不应依赖上游可信。
func TestProxiedRefusesForeignHosts(t *testing.T) {
	c := &Client{ProxyBase: "https://gh-proxy.com"}
	for _, in := range []string{
		"https://evil.example.com/x",
		"http://github.com/o/r", // 非 https
		"https://notgithub.com/o/r",
		"https://github.com.evil.example.com/o/r",
	} {
		if got := c.proxied(in); got != in {
			t.Errorf("proxied(%q) 不该改写，实际 %q", in, got)
		}
	}
}

// TestProxiedDisabledIsPassthrough 未配置代理时原样返回。
func TestProxiedDisabledIsPassthrough(t *testing.T) {
	c := &Client{}
	raw := "https://github.com/o/r/releases/download/v1/a"
	if got := c.proxied(raw); got != raw {
		t.Fatalf("未配置代理时应原样返回，实际 %q", got)
	}
}

// TestManagerProxySentinel 空值走默认代理，"-" 表示显式直连。
//
// 留空与「不想要代理」无法区分，所以用一个不可能与真实地址冲突的哨兵值。
func TestManagerProxySentinel(t *testing.T) {
	if got := NewManager(Options{Proxy: ""}).client.ProxyBase; got != DefaultProxyBase {
		t.Fatalf("空值应回落到默认代理，实际 %q", got)
	}
	if got := NewManager(Options{Proxy: "-"}).client.ProxyBase; got != "" {
		t.Fatalf(`"-" 应表示直连，实际 %q`, got)
	}
	custom := "https://my-proxy.example"
	if got := NewManager(Options{Proxy: custom}).client.ProxyBase; got != custom {
		t.Fatalf("自定义代理未生效，实际 %q", got)
	}
}

// TestDefaultProxyIsHTTPS 默认代理必须是 https。
func TestDefaultProxyIsHTTPS(t *testing.T) {
	if !strings.HasPrefix(DefaultProxyBase, "https://") {
		t.Fatalf("默认代理必须走 https，实际 %q", DefaultProxyBase)
	}
}
