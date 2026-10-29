// ═══ 更新日志 ═══
// 2026-10-29：新增。锁住「断词已失效、改写才有效」的实测结论。
package upstream

import (
	"bytes"
	"encoding/json"
	"testing"
)

// 2026-10-29 实测矩阵（cn 域，同一账号，每次只改一个变量）：
//
//	x-anthropic-billing-header 原样              → 11128
//	插零宽 x-a-nthropic-b-illing-h-eader       → 11128  ← 断词已失效
//	插普通空格 x - anthropic - billing - header → 11128
//	去掉 x- 前缀 → anthropic-billing-header      → 200
//	换成中文「计费头：」                         → 200
//
// 权威数据见 docs/channel-verification.md。这个测试保证「改写」路径永远不会
// 退化成插零宽——那已经被上游识别了。
func TestBillingHeaderRewrittenNotWordBroken(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` +
		"x-anthropic-billing-header: cc_version=2.1.312.03a; cc_entrypoint=cli;" +
		`"},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("计费头必须在发送前改写")
	}
	// 核心断言：原样触发串必须消失
	if bytes.Contains(out, []byte("x-anthropic-billing-header")) {
		t.Fatalf("原样触发串仍在: %s", out)
	}
	// 且必须是「删前缀」而不是「插零宽」——插零宽对上游已无效
	if !bytes.Contains(out, []byte("anthropic-billing-header")) {
		t.Fatalf("未按预期删掉 x- 前缀: %s", out)
	}
	if bytes.Contains(out, []byte("x​-anthropic")) {
		t.Fatal("退回了插零宽的旧方案（上游已能识别）")
	}
}

// 归属句必须整句大写化：实测「改措辞」「去掉 Claude」都能通过，
// 插零宽不行。这里锁住大写化形态。
func TestAttributionSentenceUppercased(t *testing.T) {
	sentence := "You are Claude Code, Anthropic's official CLI for Claude"
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` +
		sentence + `"},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("归属句必须被改写")
	}
	if bytes.Contains(out, []byte(sentence)) {
		t.Fatalf("归属句原样存留: %s", out)
	}
	// 2026-10-29 生产 dump 实测：大写化无效（上游大小写不敏感），必须改措辞。
	if !bytes.Contains(out, []byte("You are a coding assistant, an official CLI for Claude")) {
		t.Fatalf("未按预期替换成中性措辞: %s", out)
	}
}

// 改写必须幂等：多档重试会对同一 body 反复调用，输出不能累积变化。
func TestRewriteIsIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` +
		"x-anthropic-billing-header: v2; You are Claude Code, Anthropic's official CLI for Claude" +
		`"},{"role":"user","content":"hi"}]}`)

	first, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("首轮应发生改写")
	}
	second, changedAgain := NeutralizeBillingHeaders(first)
	if changedAgain {
		t.Fatalf("第二轮不应再报改动（不幂等）: %s", second)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("两轮输出不一致:\n1=%s\n2=%s", first, second)
	}
}

// 零误伤：不含触发串的首行一个字节都不许动。
//
// 零宽剥离只用于**匹配**，不能落回输出——否则「客户端自己带的零宽字符」
// 会因为一次无关的中性化尝试而被清掉，违反自 2026-10-17 起的零误伤契约。
func TestNoTriggerMeansNoByteChanged(t *testing.T) {
	original := "totally harmless line with no trigger token"
	body := []byte(`"model":"m","messages":[{"role":"system","content":"` +
		original + `\nrest"},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeBillingHeaders(body)
	if changed {
		t.Fatalf("无触发串却被改动: %s", out)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("无触发串时 body 必须逐字节不变:\n got=%s\nwant=%s", out, body)
	}
}

// 结构完整性：改写后 body 仍是合法 JSON，且非目标字段全部保留。
func TestRewriteKeepsJSONValid(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"` +
		"x-anthropic-billing-header: v2" +
		`"},{"role":"user","content":"hi"}],"stream":true,"max_tokens":9}`)

	out, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("应发生改写")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	if obj["stream"] != true || obj["max_tokens"] != float64(9) || obj["model"] != "m" {
		t.Fatalf("非目标字段被改动: %v", obj)
	}
}

// 2026-10-29 生产 dump 实测：Claude Code 2.1.312 发来的归属句**本身就是大写 +
// 词间插零宽**（"Y\u200bOU A\u200bRE C\u200bLAUDE ..."）。按小写字面量匹配永远
// 命不中，改写因此从未生效——三档重试全被拒。这里用真实形态做回归。
func TestUppercaseZeroWidthAttributionIsRewritten(t *testing.T) {
	// 逐字符还原客户端真实字节：大写 + 每个词首后插 U+200B
	raw := "Y\u200bOU A\u200bRE C\u200bLAUDE C\u200bODE, A\u200bNTHROPIC'S\u200b " +
		"O\u200bFFICIAL C\u200bLI F\u200bOR C\u200bLAUDE."
	body := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"` +
		raw + `"}]},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("大写+零宽的归属句必须被改写（否则上游仍 11128）")
	}
	if !bytes.Contains(out, []byte("coding assistant")) {
		t.Fatalf("未替换成中性措辞: %s", out)
	}
	if bytes.Contains(out, []byte("LAUDE")) {
		t.Fatalf("原大写句仍存留: %s", out)
	}
}

// 2026-10-29（另一台设备生产实测补充）：DeepSeek Harness 的归属句与
// 带 @ 的包名标识同样命中 11128。两条都必须能改写。
func TestHarnessAndAtTokensAreRewritten(t *testing.T) {
	cases := []struct {
		name, body, mustVanish, mustAppear string
	}{
		{
			name:       "harness 归属句",
			body:       `{"model":"m","messages":[{"role":"system","content":"You are DeepSeek Harness, an official coding agent."},{"role":"user","content":"hi"}]}`,
			mustVanish: "You are DeepSeek Harness",
			mustAppear: "You are a coding assistant",
		},
		{
			name:       "@anthropic-ai 首行",
			body:       `{"model":"m","messages":[{"role":"system","content":"Use @anthropic-ai/sdk here."},{"role":"user","content":"hi"}]}`,
			mustVanish: "@anthropic-ai",
			mustAppear: "anthropic-ai",
		},
		{
			// 关键用例：@ 包名出现在 system 正文中段（skill 清单、工具描述），
			// 不在首行。只走首行档会完全漏掉，必须走全文路径。
			name:       "@anthropic-ai 非首行",
			body:       `{"model":"m","messages":[{"role":"system","content":"line one\n\nskills: @anthropic-ai, @deepseek-ai"},{"role":"user","content":"hi"}]}`,
			mustVanish: "@anthropic-ai",
			mustAppear: "anthropic-ai",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := NeutralizeBillingHeaders([]byte(tc.body))
			if !changed {
				t.Fatalf("未发生改写: %s", tc.body)
			}
			if bytes.Contains(out, []byte(tc.mustVanish)) {
				t.Fatalf("触发串仍在: %s", out)
			}
			if !bytes.Contains(out, []byte(tc.mustAppear)) {
				t.Fatalf("未替换成预期形态: %s", out)
			}
			if bytes.Contains(out, []byte(wafBreakMarker)) {
				t.Fatalf("退回插零宽（上游已识别）: %s", out)
			}
		})
	}
}

// 零误伤：正文里没有这些串时，一个字节都不许动。
func TestAnywhereTokensLeaveCleanBodyUntouched(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"A normal system prompt with no triggers."},{"role":"user","content":"hi"}]}`)
	out, changed := NeutralizeBillingHeaders(body)
	if changed {
		t.Fatalf("无触发串却被改动: %s", out)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("无触发串时 body 必须逐字节不变")
	}
}
