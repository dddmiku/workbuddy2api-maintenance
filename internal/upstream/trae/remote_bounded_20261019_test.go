package trae

import (
	"encoding/json"
	"strings"
	"testing"
)

// 2026-10-19 生产事故的回归：Claude Code 的 2.4MB 请求被 Remote 空流打成 502。
//
// 真实失败形态（从生产日志还原）：
//
//	query 1,101,077 字符 / content 1,100,958 字符
//	query 只有 2 段，都是 system：934,956 + 52,051
//	→ boundedRemoteQuery 无物可删（nonSystem==0），原样返回
//	→ 987K > 460K 上限 → 上游静默空流 → 502
//
// 判据必须落在「压不进上限」而不是「体积大」：小请求照常走 Remote。
func TestRemoteQueryUnboundedDetectsSystemOnlyContext(t *testing.T) {
	// 模拟 Claude Code：超大 system 提示 + 一条用户消息。
	bigSystem := strings.Repeat("You are a coding assistant. Follow every rule below.\n", 17000)
	user := "只回复 OK"
	body := map[string]any{
		"model": "trae:deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": bigSystem},
			map[string]any{"role": "system",
				"content": "x-anthropic-billing-header: cc_version=2.1.302.daa; " +
					strings.Repeat("padding ", 6000)},
			map[string]any{"role": "user", "content": user},
		},
	}
	prepared, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared=%d bytes", len(prepared))

	if !remoteQueryUnbounded(prepared) {
		t.Fatal("超大 system 上下文必须判定为压不进 Remote 上限")
	}
}

// 正常大小的请求不能被误判（否则所有请求都会绕开 Remote）。
func TestRemoteQueryUnboundedAllowsNormalRequests(t *testing.T) {
	body := map[string]any{
		"model": "trae:deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "只回复 OK"},
		},
	}
	prepared, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if remoteQueryUnbounded(prepared) {
		t.Fatal("普通请求不该被判定为压不进上限")
	}
}

// 多轮对话即使总字数很大，只要裁剪能压进上限就不该换端点——
// boundedRemoteQuery 正是靠丢弃早期轮次工作的。
func TestRemoteQueryUnboundedAllowsTrimmableConversation(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are a helpful assistant."},
	}
	// 40 轮，每轮 8K 字符 —— 总量远超 460K，但可成对丢弃。
	filler := strings.Repeat("historical context line.\n", 320)
	for i := 0; i < 40; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": filler},
			map[string]any{"role": "assistant", "content": filler})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "最后的问题"})

	prepared, err := json.Marshal(map[string]any{
		"model":    "trae:deepseek-v4.1-flash",
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared=%d bytes", len(prepared))

	if remoteQueryUnbounded(prepared) {
		t.Fatal("可裁剪的多轮对话不该被判定为压不进上限（会白白绕开 Remote）")
	}
}
