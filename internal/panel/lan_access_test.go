package panel

// 2026-10-15：局域网访问开关的测试。
//
// 三条核心路径：
//  1. 无调用密钥时拒绝开启，且**不写文件**（安全闸门，见 SetLANAccess 注释）；
//  2. 有密钥时开启写入 0.0.0.0:<port> + admin_allow_cidrs；关闭写回 127.0.0.1 并清 CIDR；
//  3. LANAccessState 的判定口径（非回环即开启）与 NetworksAllow 的白名单行为。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSetLANAccessRequiresKey 无密钥拒绝开启，配置文件保持原样。
func TestSetLANAccessRequiresKey(t *testing.T) {
	path := writeTestConfig(t, `{
  "listen": "127.0.0.1:7863",
  "api_key": "",
  "api_keys_file": "./data/api_keys.json"
}`)
	before, _ := os.ReadFile(path)
	ok, msg := SetLANAccess(path, true, "")
	if ok {
		t.Fatal("无 api_key 且无密钥库时应当拒绝开启")
	}
	if !strings.Contains(msg, "密钥") {
		t.Errorf("拒绝文案应说明需要密钥：%s", msg)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("拒绝时不得写文件")
	}
}

// TestSetLANAccessKeyStoreCounts 密钥库文件存在即可开启（面板密钥页建的第一把密钥落它）。
func TestSetLANAccessKeyStoreCounts(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "api_keys.json")
	if err := os.WriteFile(store, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTestConfig(t, `{
  "listen": "127.0.0.1:7863",
  "api_key": "",
  "api_keys_file": "`+filepath.ToSlash(store)+`"
}`)
	ok, msg := SetLANAccess(path, true, "192.168.5.0/24")
	if !ok {
		t.Fatalf("密钥库存在时应允许开启，got %s", msg)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"0.0.0.0:7863"`) {
		t.Errorf("开启后 listen 应为 0.0.0.0:7863：%s", raw)
	}
	if !strings.Contains(string(raw), `"admin_allow_cidrs": "192.168.5.0/24"`) {
		t.Errorf("admin_allow_cidrs 应写入：%s", raw)
	}
}

// TestSetLANAccessRoundTrip 开启→关闭的完整往返：端口保留、CIDR 清除。
func TestSetLANAccessRoundTrip(t *testing.T) {
	path := writeTestConfig(t, `{
  "listen": "127.0.0.1:7864",
  "api_key": "sk-test"
}`)
	// 自定义端口要保留。
	if ok, msg := SetLANAccess(path, true, ""); !ok {
		t.Fatalf("开启失败：%s", msg)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"0.0.0.0:7864"`) {
		t.Errorf("开启应保留原端口 7864：%s", raw)
	}
	// 关闭：回环 + 忽略传入的 CIDR（关闭语义 = 回到只本机，白名单一并清除）。
	if ok, msg := SetLANAccess(path, false, "192.168.0.0/16"); !ok {
		t.Fatalf("关闭失败：%s", msg)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), `"127.0.0.1:7864"`) {
		t.Errorf("关闭后 listen 应回 127.0.0.1:7864：%s", raw)
	}
	if strings.Contains(string(raw), `"admin_allow_cidrs"`) {
		t.Errorf("关闭应删除 admin_allow_cidrs（无论传什么）：%s", raw)
	}
	// 幂等：再关一次报状态未变化。
	ok, msg := SetLANAccess(path, false, "")
	if !ok || msg != "状态未变化" {
		t.Errorf("重复关闭应报状态未变化，got ok=%v msg=%s", ok, msg)
	}
}

// TestLANAccessStateDetection 判定口径：非回环即开启。
func TestLANAccessStateDetection(t *testing.T) {
	cases := []struct {
		listen  string
		enabled bool
	}{
		{"127.0.0.1:7863", false},
		{"localhost:7863", false},
		{"0.0.0.0:7863", true},
		{"192.168.5.65:7863", true},
		{":7863", false}, // 无 host 部分按默认（容器形态 listen ":port" 由部署层约束）
	}
	for _, c := range cases {
		path := writeTestConfig(t, `{"listen": "`+c.listen+`"}`)
		enabled, _ := LANAccessState(path)
		if enabled != c.enabled {
			t.Errorf("listen=%s enabled=%v want %v", c.listen, enabled, c.enabled)
		}
	}
}

// TestNetworksAllow 白名单纯函数：空放行、回环放行、段内放行、段外拒绝。
func TestNetworksAllow(t *testing.T) {
	const cidrs = "192.168.5.0/24,10.0.0.0/8"
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.5.65", true},
		{"192.168.5.1", true},
		{"10.1.2.3", true},
		{"192.168.6.1", false},
		{"172.16.0.1", false},
		{"8.8.8.8", false},
		{"127.0.0.1", true}, // 回环始终放行
	}
	for _, c := range cases {
		if got := NetworksAllow(cidrs, c.ip); got != c.want {
			t.Errorf("NetworksAllow(%q, %q)=%v want %v", cidrs, c.ip, got, c.want)
		}
	}
	if !NetworksAllow("", "8.8.8.8") {
		t.Error("未配置白名单应全部放行")
	}
	if !NetworksAllow("not-a-cidr", "8.8.8.8") {
		t.Error("全非法配置视同未配置（放行），不能把运维锁在门外")
	}
}

// TestListenPortOf 端口解析的四种形态。
func TestListenPortOf(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:7863": "7863",
		"0.0.0.0:7864":   "7864",
		":7865":          "7865",
		"7866":           "7866",
		"":               "7863",
		"garbage":        "7863",
	}
	for in, want := range cases {
		if got := listenPortOf(in); got != want {
			t.Errorf("listenPortOf(%q)=%q want %q", in, got, want)
		}
	}
}
