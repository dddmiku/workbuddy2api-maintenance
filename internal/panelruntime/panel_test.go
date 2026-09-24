// ═══ 更新日志 ═══
// 2026-09-25：使用隔离凭据目录验证内嵌面板、进程恢复和反向代理信任边界。
package panelruntime

import (
	"io"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedPanelLifecycleAndRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("embedded runtime targets Linux containers; Windows Python has no UnixStreamServer")
	}
	python := os.Getenv("WB2A_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 unavailable")
		}
	}
	base := t.TempDir()
	config := filepath.Join(base, "config.json")
	if err := os.WriteFile(config, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Start(Options{Python: python, BaseDir: base, ConfigPath: config, AuthDir: filepath.Join(base, "auths"), AdminDir: filepath.Join(base, "panel-data"), AdminSocket: filepath.Join(base, "missing.sock"), RuntimeDir: base})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	public := httptest.NewServer(r)
	defer public.Close()
	for _, path := range []string{"/admin/__health", "/admin/login"} {
		resp, err := public.Client().Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("panel unavailable: %d %s", resp.StatusCode, raw)
		}
	}
	r.mu.Lock()
	old := r.cmd.Process
	err = old.Kill()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		r.mu.Lock()
		pid := r.cmd.Process.Pid
		r.mu.Unlock()
		if pid == old.Pid {
			continue
		}
		resp, err := public.Client().Get(public.URL + "/admin/__health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
	}
	t.Fatal("embedded panel did not recover after child exit")
}

func TestPanelProxyForwardingTrust(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		r := &Runtime{opts: Options{TrustedProxies: "127.0.0.1/32"}}
		in := httptest.NewRequest("GET", "http://console.example/admin/api/state", nil)
		in.Header.Set("X-Real-IP", "203.0.113.7")
		in.Header.Set("X-Forwarded-Proto", "https")
		in.Header.Set("Forwarded", "for=forged")
		in.Header.Set("CF-Connecting-IP", "forged")
		in.RemoteAddr = "198.51.100.9:1234"
		if trusted {
			in.RemoteAddr = "127.0.0.1:1234"
		}
		out := in.Clone(in.Context())
		request := &httputil.ProxyRequest{In: in, Out: out}
		r.newProxy().Rewrite(request)
		wantIP := "198.51.100.9"
		wantProto := "http"
		if trusted {
			wantIP = "203.0.113.7"
			wantProto = "https"
		}
		if out.Host != in.Host || out.URL.Path != "/api/state" || out.Header.Get("X-Real-IP") != wantIP || out.Header.Get("X-Forwarded-Proto") != wantProto || out.Header.Get("Forwarded") != "" || strings.Contains(out.Header.Get("CF-Connecting-IP"), "forged") {
			t.Fatalf("proxy lost origin or trusted a forged header: %+v", out)
		}
	}
}
