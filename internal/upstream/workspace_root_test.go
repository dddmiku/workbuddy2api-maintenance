package upstream

// 2026-11-04：`workspace root` 在 system 消息里被判 11128 的回归用例。
//
// 现场：DSH 客户端 19:00 连续 9 次重试、换 3 个账号全部被拒。单变量回放定位到
// system 第 105 行的这个短语（详见 channelFirstLineTriggerTokens 的矩阵）。
// 关键约束：
//   - 与行号无关（在 system 任意行都触发），所以必须对整条 system 处理；
//   - 只在 system 里触发：官方客户端在 user 消息的 IDENTITY.md 模板里也发它（抓包 6/6）
//     且能 200，所以 user 侧一个字节都不能动。

import (
	"encoding/json"
	"strings"
	"testing"
)

func systemContentOf(t *testing.T, out []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, raw := range obj["messages"].([]any) {
		m := raw.(map[string]any)
		if strings.EqualFold(m["role"].(string), "system") {
			return m["content"].(string)
		}
	}
	t.Fatal("没有 system 消息")
	return ""
}

// TestWorkspaceRootInSystemNeutralized system 任意行的触发短语都要被改写。
func TestWorkspaceRootInSystemNeutralized(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"line1\n` +
		`line105: Tokens prefixed with @ are paths the user explicitly referenced. ` +
		`Relative paths resolve from the workspace root; absolute paths identify files."},` +
		`{"role":"user","content":"hi"}]}`)
	out, changed := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	if !changed {
		t.Fatal("changed=false：system 里的触发短语未被处理")
	}
	got := systemContentOf(t, out)
	if strings.Contains(got, "workspace root") {
		t.Fatalf("system 仍含触发短语: %s", got)
	}
	if !strings.Contains(got, "workspace base") {
		t.Fatalf("未替换成 workspace base: %s", got)
	}
}

// TestWorkspaceRootInUserUntouched user 消息里的同一短语必须原样保留。
//
// 依据：官方客户端 6 份抓包全部在 user 消息的 IDENTITY.md 模板里发
// "Save this file at the workspace root as `IDENTITY.md`" 且请求成功——
// 上游只对 system 里的它敏感。改 user 侧只会白白改变用户可见内容。
func TestWorkspaceRootInUserUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"clean system"},` +
		`{"role":"user","content":"save at the workspace root as IDENTITY.md"}]}`)
	out, _ := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	if !strings.Contains(string(out), "workspace root") {
		t.Fatal("user 消息里的 workspace root 不该被改写")
	}
}

// TestWorkspaceRootCaseInsensitive 大小写不敏感：实测 `workspace rooT` 同样被拒。
func TestWorkspaceRootCaseInsensitive(t *testing.T) {
	for _, variant := range []string{"Workspace Root", "WORKSPACE ROOT", "workspace Root"} {
		body := []byte(`{"model":"m","messages":[{"role":"system","content":"resolve from the ` +
			variant + ` please"},{"role":"user","content":"hi"}]}`)
		out, _ := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
		if strings.Contains(strings.ToLower(string(out)), "workspace root") {
			t.Fatalf("%q 未被改写: %s", variant, string(out))
		}
	}
}

// TestWorkspaceRootIdempotent 二次处理不再产生改动（避免多档重试反复改写）。
func TestWorkspaceRootIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"from the workspace root here"},` +
		`{"role":"user","content":"hi"}]}`)
	first, changed1 := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	if !changed1 {
		t.Fatal("首次应改动")
	}
	second, changed2 := NeutralizeChannelTriggerAt(first, channelLevelFingerprint)
	if changed2 {
		t.Fatal("第二次仍报改动（不幂等）")
	}
	if string(second) != string(first) {
		t.Fatal("第二次内容变了")
	}
}
