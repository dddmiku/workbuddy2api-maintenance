// ═══ 更新日志 ═══
// 2026-09-22：补 features.reasoning_loop_stop_only 的配置回归：缺省为 false（保持
//
//	命中后同账号重发），显式 true 才切到「只停不重发」。
//
// 2026-09-19：确认旧配置默认开启重复推理保护、明确false可关闭，非法值不被静默接受。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReasoningLoopGuardConfiguration(t *testing.T) {
	if !Default().Features.ReasoningLoopGuard {
		t.Fatal("default reasoning loop guard is disabled")
	}
	for _, tc := range []struct {
		name, body string
		want       bool
		invalid    bool
	}{
		{name: "old_config", body: `{}`, want: true},
		{name: "old_features", body: `{"features":{"sanitize_blacklist_fingerprints":false}}`, want: true},
		{name: "old_null_features", body: `{"features":null}`, want: true},
		{name: "enabled", body: `{"features":{"reasoning_loop_guard":true}}`, want: true},
		{name: "disabled", body: `{"features":{"reasoning_loop_guard":false}}`},
		{name: "invalid_string", body: `{"features":{"reasoning_loop_guard":"false"}}`, invalid: true},
		{name: "invalid_number", body: `{"features":{"reasoning_loop_guard":0}}`, invalid: true},
		{name: "invalid_null", body: `{"features":{"reasoning_loop_guard":null}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid guard option was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Features.ReasoningLoopGuard != tc.want {
				t.Fatalf("guard=%t want %t", cfg.Features.ReasoningLoopGuard, tc.want)
			}
		})
	}
}

// TestReasoningLoopStopOnlyConfiguration 锁定「命中之后怎么办」这个开关的缺省语义：
// 不配置时必须是 false（保持用户无感的重发），只有显式 true 才切成只停不重发。
func TestReasoningLoopStopOnlyConfiguration(t *testing.T) {
	if Default().Features.ReasoningLoopStopOnly {
		t.Fatal("default reasoning loop stop-only is enabled; the retry must stay the default")
	}
	for _, tc := range []struct {
		name, body string
		want       bool
		invalid    bool
	}{
		{name: "old_config", body: `{}`},
		{name: "retry_default", body: `{"features":{"reasoning_loop_stop_only":false}}`},
		{name: "stop_only", body: `{"features":{"reasoning_loop_stop_only":true}}`, want: true},
		{name: "invalid_string", body: `{"features":{"reasoning_loop_stop_only":"true"}}`, invalid: true},
		{name: "invalid_number", body: `{"features":{"reasoning_loop_stop_only":1}}`, invalid: true},
		{name: "invalid_null", body: `{"features":{"reasoning_loop_stop_only":null}}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid stop-only option was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Features.ReasoningLoopStopOnly != tc.want {
				t.Fatalf("stop_only=%t want %t", cfg.Features.ReasoningLoopStopOnly, tc.want)
			}
		})
	}
}
