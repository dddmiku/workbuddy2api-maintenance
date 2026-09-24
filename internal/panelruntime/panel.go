// ═══ 更新日志 ═══
// 2026-09-25：网关托管同版管理台的私有Unix进程，启动就绪后才服务并在优雅退出后回收。
package panelruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/runlog"
	"workbuddy2api/panel"
)

type Options struct {
	ConfigPath, BaseDir, AuthDir, AdminDir, AdminSocket, RuntimeDir, LogPath, Python string
	TrustedProxies                                                                   string
}

type Runtime struct {
	dir, socket string
	opts        Options
	cancel      context.CancelFunc
	done        chan struct{}
	transport   *http.Transport
	proxy       *httputil.ReverseProxy
	mu          sync.Mutex
	cmd         *exec.Cmd
}

func Start(opts Options) (*Runtime, error) {
	if opts.Python == "" {
		opts.Python = "python3"
	}
	dir, err := os.MkdirTemp("", "wb2panel-")
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(panel.Files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(path, 0700)
		}
		data, err := panel.Files.ReadFile(name)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0600)
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runtime{dir: dir, socket: filepath.Join(dir, "panel.sock"), opts: opts, cancel: cancel, done: make(chan struct{})}
	r.transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.socket)
	}, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 4 * time.Minute}
	r.proxy = r.newProxy()
	ready := make(chan error, 1)
	go r.supervise(ctx, ready)
	if err := <-ready; err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Runtime) command() *exec.Cmd {
	// #nosec G204 -- Python is an administrator setting; the script is embedded in the current executable.
	cmd := exec.Command(r.opts.Python, "-u", filepath.Join(r.dir, "app.py"))
	overrides := map[string]string{
		"WB2API_RUNTIME": "native", "WB2API_GATEWAY_DIR": r.opts.BaseDir, "WB2API_CONFIG_PATH": r.opts.ConfigPath,
		"WB2API_AUTHS_DIR": r.opts.AuthDir, "WB2API_ADMIN_DIR": r.opts.AdminDir, "WB2API_ADMIN_SOCKET": r.opts.AdminSocket,
		"WB2API_RUNTIME_DIR": r.opts.RuntimeDir, "WB2API_PANEL_SOCKET": r.socket, "WB2API_LOG_PATH": r.opts.LogPath,
		"WB2API_TRUSTED_PROXIES": "127.0.0.1/32,::1/128", "PYTHONDONTWRITEBYTECODE": "1", "PYTHONUNBUFFERED": "1",
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	for key, value := range overrides {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Dir = r.opts.BaseDir
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = runlog.Output(os.Stdout)
	cmd.Stderr = runlog.Output(os.Stderr)
	configureProcess(cmd)
	return cmd
}

func (r *Runtime) supervise(ctx context.Context, first chan<- error) {
	defer close(r.done)
	defer os.RemoveAll(r.dir)
	initial := true
	for {
		if ctx.Err() != nil {
			if initial {
				first <- ctx.Err()
			}
			return
		}
		_ = os.Remove(r.socket)
		cmd := r.command()
		if err := cmd.Start(); err != nil {
			if initial {
				first <- err
				return
			}
			log.Printf("[panel] restart failed: %v", err)
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		err := r.waitReady(ctx, exited)
		if err != nil {
			stopProcess(cmd)
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				killProcess(cmd)
				<-exited
			}
			if initial {
				first <- err
				return
			}
			if !wait(ctx, time.Second) {
				return
			}
			continue
		}
		if initial {
			first <- nil
			initial = false
		}
		select {
		case <-ctx.Done():
			stopProcess(cmd)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				killProcess(cmd)
				<-exited
			}
			return
		case err := <-exited:
			// A Python request thread may have started a helper before the
			// panel crashed. Reap that private group before starting its successor.
			killProcess(cmd)
			log.Printf("[panel] process exited (%v); restarting embedded panel", err)
		}
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Runtime) waitReady(ctx context.Context, exited chan error) error {
	deadline := time.Now().Add(15 * time.Second)
	client := &http.Client{Transport: r.transport, Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			exited <- err
			return fmt.Errorf("panel exited before ready: %v", err)
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/__health", nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if !wait(ctx, 50*time.Millisecond) {
			return ctx.Err()
		}
	}
	return errors.New("embedded panel did not become ready")
}

func (r *Runtime) Close() { r.cancel(); <-r.done; r.transport.CloseIdleConnections() }

func (r *Runtime) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.proxy.ServeHTTP(w, req) }

func (r *Runtime) newProxy() *httputil.ReverseProxy {
	networks := []*net.IPNet{}
	for _, part := range strings.Split(r.opts.TrustedProxies, ",") {
		if _, network, err := net.ParseCIDR(strings.TrimSpace(part)); err == nil {
			networks = append(networks, network)
		}
	}
	target := &url.URL{Scheme: "http", Host: "localhost"}
	return &httputil.ReverseProxy{Transport: r.transport, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(target)
		p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, "/admin")
		p.Out.URL.RawPath = ""
		p.Out.Host = p.In.Host
		peer, _, err := net.SplitHostPort(p.In.RemoteAddr)
		if err != nil {
			peer = p.In.RemoteAddr
		}
		ip := net.ParseIP(peer)
		trusted := false
		for _, network := range networks {
			if network.Contains(ip) {
				trusted = true
				break
			}
		}
		clientIP := peer
		if trusted {
			if forwarded := net.ParseIP(p.In.Header.Get("X-Real-IP")); forwarded != nil {
				clientIP = forwarded.String()
			}
		}
		p.Out.Header.Del("Forwarded")
		p.Out.Header.Del("CF-Connecting-IP")
		p.Out.Header.Set("X-Real-IP", clientIP)
		p.Out.Header.Set("X-Forwarded-For", clientIP)
		proto := "http"
		if p.In.TLS != nil || trusted && p.In.Header.Get("X-Forwarded-Proto") == "https" {
			proto = "https"
		}
		p.Out.Header.Set("X-Forwarded-Proto", proto)
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("[panel] local proxy unavailable: %v", err)
		http.Error(w, "administration temporarily unavailable", http.StatusBadGateway)
	}}
}
