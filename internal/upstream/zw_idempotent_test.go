package upstream

import (
	"bytes"
	"strings"
	"testing"
)

// ccBillingFirstLine 构造 Claude Code 2.1.301 真实形态的系统提示首行：
// 每个单词首字母后带一个 Anthropic 自己插的 U+200B，词表里存的是纯 ASCII。
func ccBillingFirstLine() string {
	zw := "​"
	return "x" + zw + "-a" + zw + "nthropic-b" + zw + "illing-h" + zw + "eader: " +
		"cc_version=2.1.301.fb5; cc_entrypoint=cli; " +
		"You are " + zw + "Claude Code, " + zw + "Anthropic's " + zw + "official CLI for " + zw + "Claude."
}

// maxZeroWidthRun 返回文本中连续零宽字符的最大长度。
func maxZeroWidthRun(text string) int {
	run, maxRun := 0, 0
	for _, r := range text {
		if r == '​' {
			run++
			if run > maxRun {
				maxRun = run
			}
			continue
		}
		run = 0
	}
	return maxRun
}

// TestNeutralizeSameLevelIsIdempotent 锁住「同一档重复施加不改变输出」。
//
// 为什么必需（2026-10-17 生产落盘实测）：网关会遇到**同一份已中和过的 body 再次
// 经过同一档中和**的情况——计费头在发送前就预先断词（NeutralizeBillingHeaders），
// 被拒后重试又从第 0 档开始（NeutralizeChannelTriggerAt(prepared, 0)），
// 于是同一段首行被第 0 档处理了两次。修复前每处理一次就在同一位置再插一个零宽，
// 生产 dump 里抓到的真实形态是每个词首 5 个 U+200B：
//
//	'x[ZW×5]-a[ZW×5]nthropic-b[ZW×5]illing-h[ZW×5]eader: ...'
//
// 这种「被反复篡改」的形态本身就是异常特征——等于把「非官方客户端」写在脸上，
// 上游的渠道校验照样能据此挑出来。
//
// 注意这里只断言**同一档**的幂等性。跨档升级（第 2 档 = 正文全量断词）本来就该
// 产生更多断点，那不是累积而是设计。
func TestNeutralizeSameLevelIsIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		jsonString(ccBillingFirstLine()+"\n\nrest of the prompt") +
		`},{"role":"user","content":"hi"}]}`)

	for _, level := range []int{channelLevelFingerprint, channelLevelAttribution, channelLevelAll} {
		first, changed := NeutralizeChannelTriggerAt(body, level)
		if !changed && level != channelLevelFingerprint {
			t.Fatalf("level %d 未产生任何改动", level)
		}
		second, changedAgain := NeutralizeChannelTriggerAt(first, level)
		// 第 2 档必然有改动（正文全量断词）；其余档第 2 次应报「无改动」。
		if level != channelLevelAll && changedAgain {
			t.Fatalf("level %d 第二次仍报改动（不幂等）", level)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("level %d 重复施加后输出不同 —— 零宽字符在累积。\n第一次: %q\n第二次: %q",
				level, snippet(first), snippet(second))
		}
		if run := maxZeroWidthRun(string(second)); run > 1 {
			t.Fatalf("level %d 出现 %d 个连续零宽字符（应为 1）：%q", level, run, snippet(second))
		}
	}
}

// TestNeutralizeProductionSequenceDoesNotStack 复现生产真实调用序列：
// 发送前 NeutralizeBillingHeaders（第 0 档首行）→ 被拒 → 重试再走第 0 档。
// 两趟下来首行形态必须一致。
func TestNeutralizeProductionSequenceDoesNotStack(t *testing.T) {
	zw := "​"
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		jsonString(ccBillingFirstLine()+"\n\nrest") + `},{"role":"user","content":"hi"}]}`)

	// ① 发送前的计费头预中和
	preempted, changed := NeutralizeBillingHeaders(body)
	if !changed {
		t.Fatal("计费头未被预中和")
	}
	// ② 被 11128 拒后，第 0 档补救
	retried, _ := NeutralizeChannelTriggerAt(preempted, channelLevelFingerprint)

	if !bytes.Equal(preempted, retried) {
		t.Fatalf("生产的「预中和 → 重试第 0 档」序列导致输出变化（零宽累积）：\npreempt=%q\nretry  =%q",
			snippet(preempted), snippet(retried))
	}
	if run := maxZeroWidthRun(string(retried)); run > 1 {
		t.Fatalf("出现 %d 个连续零宽字符（应为 1）：%q", run, snippet(retried))
	}
	// 断词仍生效：连续形态的触发串必须不存在。
	for _, tok := range channelFirstLineTriggerTokens {
		if bytes.Contains(retried, []byte(tok)) {
			t.Fatalf("触发串 %q 仍是连续形态（断词失效）：%q", tok, snippet(retried))
		}
	}
	_ = zw
}

// TestNeutralizeKeepsPreExistingZeroWidthWithoutTrigger 首行**不含**触发 token 时
// 必须一个字节都不改（零误伤契约）。
//
// 这条性质与幂等是一对约束，必须同时成立：
//   - 幂等要求「同一位置不重复插标记」（由 wordBreakPoints 整块跳过零宽序列、
//     以及 insertBreaks 跳过已有标记的位置保证）；
//   - 零误伤要求「没命中就原样返回」。此前实现为了让零宽不累积，在未命中时把
//     归一化结果落回去（顺手清掉客户端原文里的零宽），但这会**抹掉同一轮里
//     其它档位刚插入的断词**——指纹句的断词被擦干净，请求又带着连续触发串打上游。
//
// 因此收敛点：清理不做独立动作，只在「命中并重写该段」时顺带完成。
func TestNeutralizeKeepsPreExistingZeroWidthWithoutTrigger(t *testing.T) {
	zw := "​"
	dirty := "totally " + zw + "harmless " + zw + "line with no trigger token"
	body := []byte(`{"model":"m","messages":[{"role":"system","content":` +
		jsonString(dirty+"\nrest") + `},{"role":"user","content":"hi"}]}`)

	out, changed := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
	if changed {
		t.Fatalf("不含触发 token 的首行被改动了（违反零误伤契约）：%q", string(out))
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("body 字节发生变化：\nwant %q\ngot  %q", string(body), string(out))
	}
}

// snippet 截出便于失败信息阅读的一小段。
func snippet(body []byte) string {
	text := string(body)
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

var _ = strings.Contains
