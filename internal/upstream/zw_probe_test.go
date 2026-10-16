package upstream

import (
	"strings"
	"testing"
)

// TestClaudeCodeRealFirstLineIsZeroWidthBroken 锁住真实场景：
// Claude Code 的系统提示首行**自带零宽空格**（Anthropic 自己插的），
// 网关的中和表若是纯 ASCII 就永远匹配不到。
//
// 实测（2026-10-17）：Claude Code 2.1.301 发来的首行逐字节是
//   "x​-a​nthropic-b​illing-h​eader: cc_version=2.1.301.fb5; ..."
// 而 channelFirstLineTriggerTokens 里存的是 "x-anthropic-billing-header"。
// 两者 strings.Contains 永不匹配 → 中和失效 → 请求带着触发串打上游 → 11128。
func TestClaudeCodeRealFirstLineIsZeroWidthBroken(t *testing.T) {
	zw := "​"
	// Claude Code 真实形态（每个单词首字母后一个零宽空格）
	realFirstLine := "x" + zw + "-a" + zw + "nthropic-b" + zw + "illing-h" + zw + "eader: " +
		"cc_version=2.1.301.fb5; cc_entrypoint=cli; " +
		"You are " + zw + "Claude Code, " + zw + "Anthropic's " + zw + "official CLI for " + zw + "Claude."

	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		jsonString(realFirstLine+"\n\nbody text") + `},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	t.Logf("changed=%v", changed)
	if !changed {
		t.Fatalf("带零宽的真实 Claude Code 首行未被中和 → 网关会带着触发串打上游，必被 11128 拒。\nbody=%s", body)
	}
	// 断词是**插入**零宽而非删除字符：去掉全部零宽后文本必然还原成原文（token 当然还在）。
	// 真正该断言的是「断词确实插在了 token 内部」——把零宽去掉再比对无意义，改为：
	// 输出里必须含被零宽打断的 token 形态（即 token 的字母之间夹了零宽字符）。
	broken := false
	compact := strings.ReplaceAll(string(out), zw, "")
	_ = compact
	for _, tok := range channelFirstLineTriggerTokens {
		if strings.Contains(string(out), tok) {
			continue // 该 token 未被打断
		}
		broken = true // 原文里含它、输出里已不含连续形态 → 已被打断
	}
	if !broken {
		t.Fatalf("没有任何 token 被打断（中和无效）：%s", string(out))
	}
	// 被拒的原始形态（连续 ASCII token）必须消失
	if strings.Contains(string(out), channelFirstLineTriggerTokens[0]) {
		t.Fatalf("计费头 token 仍以连续形态存在：%s", string(out))
	}
}

// jsonString 把字符串编码成 JSON 字符串字面量（含引号）。
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString("?")
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
