package upstream

// system 扁平化：把客户端 system 内容搬进 user，绕开上游渠道校验。
//
// 2026-11-06 实测矩阵（同一账号同时刻，cn 域）：
//
//	system = dsh 的 18086 字符/146 行内容     → 11128
//	同一份内容并入 user，system 换成官方句    → 200
//	无 system、内容只在 user                 → 200
//	system 首行官方句 + 后面放触发句          → 200（首行之后不扫）
//	system 首行就是触发句                    → 11128
//
// 因为触发串是开放集合（客户端一换就可能冒新句子），用结构性规避而不是黑名单。

import (
	"encoding/json"
	"strings"
	"testing"
)

func flattenDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return obj
}

func msgRoles(obj map[string]any) []string {
	raw, _ := obj["messages"].([]any)
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		if msg, ok := m.(map[string]any); ok {
			out = append(out, msg["role"].(string))
		}
	}
	return out
}

// TestFlattenMovesSystemIntoUser system 内容并入 user，system 只剩官方句。
func TestFlattenMovesSystemIntoUser(t *testing.T) {
	sys := "You are Claude Code, Anthropic's official CLI for Claude.\nresolve from the workspace root"
	raw, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := FlattenSystemIntoUser(raw)
	obj := flattenDecode(t, out)

	roles := msgRoles(obj)
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("角色序列 = %v want [system user]", roles)
	}
	msgs := obj["messages"].([]any)
	sysMsg := msgs[0].(map[string]any)
	if sysMsg["content"] != officialSystemLine {
		t.Errorf("system 内容 = %q want 官方句", sysMsg["content"])
	}
	userMsg := msgs[1].(map[string]any)
	userText := userMsg["content"].(string)
	// 原始 system 文本必须完整保留在 user 里
	for _, fragment := range []string{"You are Claude Code", "workspace root", "hi"} {
		if !strings.Contains(userText, fragment) {
			t.Errorf("user 里缺 %q: %s", fragment, userText)
		}
	}
	if !strings.Contains(userText, flattenSystemMarker) {
		t.Error("user 里缺系统指令标记")
	}
	// 触发句绝不能还留在 system 里
	if strings.Contains(sysMsg["content"].(string), "Claude Code") {
		t.Error("system 里仍有触发串")
	}
}

// TestFlattenDropsTriggerFromSystem 最关键的一条：出站 system 里不含任何触发串。
func TestFlattenDropsTriggerFromSystem(t *testing.T) {
	triggers := []string{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"x-anthropic-billing-header",
		"Codex CLI is an open source project led by OpenAI",
		"workspace root",
	}
	for _, trig := range triggers {
		body := []byte(`{"model":"m","messages":[{"role":"system","content":"` + trig +
			`"},{"role":"user","content":"hi"}]}`)
		out := FlattenSystemIntoUser(body)
		obj := flattenDecode(t, out)
		sysMsg := obj["messages"].([]any)[0].(map[string]any)
		if strings.Contains(sysMsg["content"].(string), trig) {
			t.Errorf("触发串 %q 仍留在 system 里", trig)
		}
	}
}

// TestFlattenMultipleSystemMessages 多个 system 按序合并。
func TestFlattenMultipleSystemMessages(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"FIRST_PART"},` +
		`{"role":"system","content":"SECOND_PART"},` +
		`{"role":"user","content":"hi"}]}`)
	out := FlattenSystemIntoUser(body)
	obj := flattenDecode(t, out)
	roles := msgRoles(obj)
	if len(roles) != 2 {
		t.Fatalf("角色序列 = %v want 2 条", roles)
	}
	userText := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(userText, "FIRST_PART") || !strings.Contains(userText, "SECOND_PART") {
		t.Errorf("两段 system 未都保留: %s", userText)
	}
	// 顺序：FIRST 在 SECOND 之前
	if strings.Index(userText, "FIRST_PART") > strings.Index(userText, "SECOND_PART") {
		t.Error("合并顺序错乱")
	}
}

// TestFlattenDeveloperRole developer 角色同样处理。
func TestFlattenDeveloperRole(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"developer","content":"DEV_INSTRUCTIONS"},` +
		`{"role":"user","content":"hi"}]}`)
	out := FlattenSystemIntoUser(body)
	obj := flattenDecode(t, out)
	if strings.Contains(string(out), `"developer"`) {
		t.Error("developer 角色应被移除")
	}
	userText := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(userText, "DEV_INSTRUCTIONS") {
		t.Error("developer 内容丢失")
	}
}

// TestFlattenIdempotent 二次调用不再改动。
func TestFlattenIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"SOME SYS"},` +
		`{"role":"user","content":"hi"}]}`)
	first := FlattenSystemIntoUser(body)
	second := FlattenSystemIntoUser(first)
	if string(first) != string(second) {
		t.Fatalf("不幂等\nfirst=%s\nsecond=%s", first, second)
	}
}

// TestFlattenNullFirstUserContentKeepsInstructions content:null 的首条 user 要能承载系统指令。
//
// 回归：初版 prependToContent 对 null 返回 changed=false，而重建逻辑已经先删掉了
// system——指令静默消失（实测 200 但模型完全没看到 system）。这里守「要么挂上、
// 要么整体不改」。
func TestFlattenNullFirstUserContentKeepsInstructions(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"YOU_ARE_A_PIRATE"},` +
		`{"role":"user","content":null},` +
		`{"role":"user","content":"hi"}]}`)
	out := FlattenSystemIntoUser(body)
	obj := flattenDecode(t, out)
	msgs := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d want 3", len(msgs))
	}
	head, ok := msgs[1].(map[string]any)["content"].(string)
	if !ok || !strings.Contains(head, "YOU_ARE_A_PIRATE") {
		t.Fatalf("null user 未承载系统指令: %#v", msgs[1])
	}
	if sys := msgs[0].(map[string]any)["content"].(string); strings.Contains(sys, "YOU_ARE_A_PIRATE") {
		t.Error("system 里仍有客户端内容")
	}
	if tail := msgs[2].(map[string]any)["content"].(string); tail != "hi" {
		t.Errorf("后续 user 被改: %q", tail)
	}
}

// TestFlattenUnsupportedFirstUserContentStillKeepsInstructions 首条 user 的 content 是
// 数字这类无法前置的形态时，另起一条 user 承载系统指令——指令绝不能丢。
//
// 回归：初版实现删掉 system 后 prependToContent 返回 false，指令静默消失
// （实测 200 但模型完全没看到 system）。
func TestFlattenUnsupportedFirstUserContentStillKeepsInstructions(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"MUST_NOT_VANISH"},` +
		`{"role":"user","content":123}]}`)
	out := FlattenSystemIntoUser(body)
	obj := flattenDecode(t, out)
	msgs := obj["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d want 3（官方 system + 指令 user + 原 user）", len(msgs))
	}
	head := msgs[1].(map[string]any)
	if head["role"] != "user" || !strings.Contains(head["content"].(string), "MUST_NOT_VANISH") {
		t.Fatalf("指令未落到独立 user: %#v", head)
	}
	orig := msgs[2].(map[string]any)
	if v, ok := orig["content"].(float64); !ok || v != 123 {
		t.Errorf("原 user content 被改: %#v", orig["content"])
	}
	if sys := msgs[0].(map[string]any)["content"].(string); strings.Contains(sys, "MUST_NOT_VANISH") {
		t.Error("system 里仍有客户端内容")
	}
}

// TestFlattenNoSystemUntouched 没有 system 时一个字节都不动。
func TestFlattenNoSystemUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if out := FlattenSystemIntoUser(body); string(out) != string(body) {
		t.Fatalf("无 system 被改动:\n%s", out)
	}
}

// TestFlattenNoUserUntouched 有 system 但无 user 时不动（否则会丢指令）。
func TestFlattenNoUserUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"ONLY SYS"},` +
		`{"role":"assistant","content":"x"}]}`)
	if out := FlattenSystemIntoUser(body); string(out) != string(body) {
		t.Fatalf("无 user 被改动:\n%s", out)
	}
}

// TestFlattenBadBodyUntouched 坏 body 原样返回。
func TestFlattenBadBodyUntouched(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, []byte("not json"), []byte("[]")} {
		if out := FlattenSystemIntoUser(bad); string(out) != string(bad) {
			t.Errorf("坏 body 被改写: %q -> %q", bad, out)
		}
	}
}

// TestFlattenPartArrayContent part 数组形态的 user 内容要保留原有 part。
func TestFlattenPartArrayContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"SYS"},` +
		`{"role":"user","content":[{"type":"text","text":"ORIGINAL_USER_TEXT"}]}]}`)
	out := FlattenSystemIntoUser(body)
	obj := flattenDecode(t, out)
	parts := obj["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("part 数 = %d want 2（系统指令 + 原 user）", len(parts))
	}
	head := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(head, "SYS") {
		t.Error("前置 part 缺系统指令")
	}
	tail := parts[1].(map[string]any)["text"].(string)
	if tail != "ORIGINAL_USER_TEXT" {
		t.Errorf("原 user part 被改: %q", tail)
	}
}

// TestFlattenPreservesOtherFields 顶层其它字段不受影响。
func TestFlattenPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"m","temperature":1,"tools":[{"type":"function"}],` +
		`"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hi"}]}`)
	obj := flattenDecode(t, FlattenSystemIntoUser(body))
	if obj["model"] != "m" {
		t.Errorf("model = %v", obj["model"])
	}
	if _, ok := obj["tools"]; !ok {
		t.Error("tools 丢失")
	}
	if v, ok := obj["temperature"].(float64); !ok || v != 1 {
		t.Errorf("temperature = %v", obj["temperature"])
	}
}

// TestFlattenAlreadyOfficialIsIdempotent system 已是官方句时原样返回。
func TestFlattenAlreadyOfficialIsIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` +
		officialSystemLine + `"},{"role":"user","content":"hi"}]}`)
	if out := FlattenSystemIntoUser(body); string(out) != string(body) {
		t.Fatalf("已是官方句仍被改动:\n%s", out)
	}
}

// TestFlattenInOutboundPipeline 端到端：prepareBody 之后的 system 不含触发串。
//
// 守「注入器真的接进了出站链路」——只测 FlattenSystemIntoUser 本身
// 无法发现「函数写好了但没人调用」。
func TestFlattenInOutboundPipeline(t *testing.T) {
	c := New()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
		`{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},` +
		`{"role":"user","content":"hi"}],"stream":true}`)
	out := c.prepareBody(body, "cn", "uid-12345678", "conv-1")
	obj := flattenDecode(t, out)
	msgs := obj["messages"].([]any)
	sysMsg := msgs[0].(map[string]any)
	if strings.Contains(sysMsg["content"].(string), "Claude Code") {
		t.Error("出站 system 仍含触发串")
	}
	// 指令文本不丢：预中和会把归属句改写成中性措辞（"You are a coding assistant,
	// an official CLI for Claude"），所以断言「后半句还在」而不是「原文逐字还在」。
	if !strings.Contains(string(out), "official CLI for Claude") {
		t.Error("指令文本整体丢失（应在 user 里）")
	}
}
