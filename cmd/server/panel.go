// ═══ 更新日志 ═══
// 2026-09-25：统一运行布局解析，版本代码与持久化数据分离，管理台随主程序运行并使用同一配置。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/panelruntime"
	"workbuddy2api/internal/runlog"
)

type unifiedLayout struct {
	enabled bool
	panel   panelruntime.Options
}

func prepareUnified(cfg *Config, configPath string) (unifiedLayout, error) {
	if os.Getenv("WB2API_PANEL_ENABLED") != "1" {
		return unifiedLayout{}, nil
	}
	base, err := os.Getwd()
	if err != nil {
		return unifiedLayout{}, err
	}
	binary, err := os.Executable()
	if err != nil {
		return unifiedLayout{}, err
	}
	runtimeDir := filepath.Dir(binary)
	for _, name := range []string{"login", "credit", "signin_bin", "trial_bin", "activity_bin", "scripts/task_common.py"} {
		if _, err := os.Stat(filepath.Join(runtimeDir, name)); err != nil {
			return unifiedLayout{}, fmt.Errorf("incomplete runtime: %s is unavailable", name)
		}
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return unifiedLayout{}, err
	}
	authDir, err := filepath.Abs(cfg.AuthDir)
	if err != nil {
		return unifiedLayout{}, err
	}
	if cfg.APIKeysSocket == "" {
		registry := cfg.APIKeysFile
		if registry == "" {
			registry = cfg.StateFile
		}
		cfg.APIKeysSocket = filepath.Join(filepath.Dir(registry), "api_keys.sock")
	}
	adminSocket, err := filepath.Abs(cfg.APIKeysSocket)
	if err != nil {
		return unifiedLayout{}, err
	}
	adminDir := os.Getenv("WB2API_ADMIN_DIR")
	if adminDir == "" {
		adminDir = filepath.Join(base, "panel-data")
	}
	adminDir, err = filepath.Abs(adminDir)
	if err != nil {
		return unifiedLayout{}, err
	}
	// #nosec G703 -- adminDir is an absolute administrator deployment setting, not an HTTP or archive value.
	if err := os.MkdirAll(adminDir, 0700); err != nil {
		return unifiedLayout{}, err
	}
	logPath, err := filepath.Abs(filepath.Join(filepath.Dir(cfg.StateFile), "logs", "gateway.log"))
	if err != nil {
		return unifiedLayout{}, err
	}
	if err := runlog.Configure(logPath); err != nil {
		return unifiedLayout{}, err
	}
	log.SetOutput(runlog.Output(os.Stderr))
	// Only child task programs consume these variables. Their paths follow the
	// active bundle, while credentials remain in the existing mounted directory.
	if err := os.Setenv("WB2API_RUNTIME_DIR", runtimeDir); err != nil {
		return unifiedLayout{}, err
	}
	if err := os.Setenv("WB2A_AUTHS", authDir); err != nil {
		return unifiedLayout{}, err
	}
	proxies := os.Getenv("WB2API_TRUSTED_PROXIES")
	if proxies == "" {
		proxies = "127.0.0.1/32,::1/128"
	}
	return unifiedLayout{enabled: true, panel: panelruntime.Options{ConfigPath: configPath, BaseDir: base, AuthDir: authDir, AdminDir: adminDir, AdminSocket: adminSocket, RuntimeDir: runtimeDir, LogPath: logPath, Python: os.Getenv("WB2A_PYTHON"), TrustedProxies: proxies}}, nil
}

func withPanel(gateway http.Handler, panel *panelruntime.Runtime) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", panel)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
	})
	mux.Handle("/", gateway)
	return mux
}

func reloadHandler(manager *hotupdate.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// This handler is installed only on the private management socket.
		w.Header().Set("Content-Type", "application/json")
		if err := manager.Reload(r.Context()); err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}
}
