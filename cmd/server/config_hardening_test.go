// ═══ 更新日志 ═══
// 2026-10-02：锁定第二轮体检发现的三条启动配置缺陷：
//  1. listen 为空/空白必须回落默认端口，不能补成 ":"（绑定所有网卡的随机端口）；
//  2. 超大的 timeout_seconds 等必须在 normalize 阶段报错，不能溢出成负数静默禁用超时；
//  3. 非法的数值/布尔 WB2A_* 环境变量必须报错，不能静默保留默认。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestListenEmptyFallsBackToDefaultPort 空 listen 不能变成 ":"。
//
// 回归点：normalize 只做「无冒号就补冒号」，空串因此变成 ":"，net.Listen 会绑定
// 所有网卡的随机端口——日志打印的仍是原始 cfg.Listen（真实端口不可知），容器
// HEALTHCHECK 固定打 127.0.0.1:7863 必然失败。空白值此前还会变成 ": " 触发
// `lookup tcp/ : unknown port`。现在统一回落文档默认端口 ":7863"。
func TestListenEmptyFallsBackToDefaultPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"空串", `{"listen":""}`},
		{"null", `{"listen":null}`},
		{"只有空白", `{"listen":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.Listen != ":7863" {
				t.Fatalf("listen=%q want :7863（空值必须回落默认端口，不能补成 \":\"）", c.Listen)
			}
		})
	}
}

// TestListenExplicitValuesUnchanged 对照：显式端口与无冒号端口的既有归一语义不变。
func TestListenExplicitValuesUnchanged(t *testing.T) {
	for body, want := range map[string]string{
		`{"listen":":9999"}`:        ":9999",
		`{"listen":"9999"}`:         ":9999",
		`{"listen":"127.0.0.1:80"}`: "127.0.0.1:80",
	} {
		c, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(%s): %v", body, err)
		}
		if c.Listen != want {
			t.Fatalf("listen=%q want %q", c.Listen, want)
		}
	}
}

// TestTimeoutSecondsOverflowRejected 超大的 timeout_seconds / header / idle 必须报错。
//
// 回归点：time.Duration(n)*time.Second 在 n > ~9.2e9 时回绕成负数；net/http 把负的
// Timeout/ResponseHeaderTimeout 当「未设置」、idle<=0 直接返回原始流，于是超时被静默
// 禁用。现在 normalize 明确拒绝而不是让它悄悄生效。
func TestTimeoutSecondsOverflowRejected(t *testing.T) {
	for _, key := range []string{"timeout_seconds", "header_timeout_seconds", "idle_timeout_seconds"} {
		body := fmt.Sprintf(`{"upstream":{"%s":10000000000}}`, key)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Fatalf("upstream.%s=10000000000 应当报错（会溢出为负、静默禁用超时）", key)
		}
	}
}

// TestTimeoutSecondsBoundaryAccepted 边界值 9223372036（MaxInt64/1e9 的整数商）仍可加载。
func TestTimeoutSecondsBoundaryAccepted(t *testing.T) {
	c, err := Load(writeConfig(t, `{"upstream":{"timeout_seconds":9223372036}}`))
	if err != nil {
		t.Fatalf("边界值应被接受: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 9223372036 {
		t.Fatalf("timeout_seconds=%d want 9223372036", c.Upstream.TimeoutSeconds)
	}
}

// TestInvalidNumericEnvRejected 非法的数值/布尔 WB2A_* 必须报错，而不是静默忽略。
//
// 回归点：此前 `if err == nil` 无 else——`WB2A_TIMEOUT_SECONDS=abc` 启动照常、
// 日志一行没有，运维以为覆盖生效了其实没有。
func TestInvalidNumericEnvRejected(t *testing.T) {
	cases := []struct{ name, value string }{
		{"WB2A_MAX_BODY_MB", "notanumber"},
		{"WB2A_OUTBOUND_IMAGE_BUDGET_MB", "7x"},
		{"WB2A_TIMEOUT_SECONDS", "abc"},
		{"WB2A_HEADER_TIMEOUT_SECONDS", ""}, // 空串 = 未设置，不参与本断言
		{"WB2A_IDLE_TIMEOUT_SECONDS", "12s"},
		{"WB2A_PASSTHROUGH_IP", "yes"},
	}
	for _, tc := range cases {
		if tc.value == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := Load(""); err == nil {
				t.Fatalf("%s=%q 应当报错，而不是静默保留默认", tc.name, tc.value)
			}
		})
	}
}

// TestValidEnvStillApplies 对照：合法 env 照常生效。
func TestValidEnvStillApplies(t *testing.T) {
	t.Setenv("WB2A_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_PASSTHROUGH_IP", "true")
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 45 {
		t.Fatalf("timeout_seconds=%d want 45", c.Upstream.TimeoutSeconds)
	}
	if !c.Upstream.PassthroughIP {
		t.Fatal("WB2A_PASSTHROUGH_IP=true 未生效")
	}
}
