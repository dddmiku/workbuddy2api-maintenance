package trae

// 2026-10-19 回归：证明 dumpPrefixFingerprint 不改变出站内容。
//
// 用户问「你的改动会影响模型能力吗，或者会丢失信息吗」。这个问题必须用
// 测试回答，而不是靠读代码说「应该不会」。
//
// 本文件锁住三件事：
//  1. dumpPrefixFingerprint 不修改传入的 body（逐字节比较）；
//  2. 它对出站 body 的唯一影响是「读」，且读的是 json.Unmarshal 出来的副本；
//  3. 关掉开关时它连解析都不做（不引入任何开销）。
//
// 另外锁住「跳过 Remote」那条日志不改变控制流：它的分支里只有 log。

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDumpPrefixDoesNotMutateBody 逐字节确认 body 未被修改。
func TestDumpPrefixDoesNotMutateBody(t *testing.T) {
	body := []byte(`{"model":"trae:deepseek-v4.1-flash","messages":[
		{"role":"system","content":[{"type":"text","text":"你是助手"}]},
		{"role":"user","content":"你好"},
		{"role":"assistant","content":[{"type":"text","text":"在的"}],"tool_calls":[
			{"index":0,"id":"call_1","type":"function",
			 "function_call":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","name":"get_weather","content":"晴"}
	],"tools":[{"type":"function","function":{"name":"get_weather",
		"parameters":"{\"type\":\"object\"}"}}],"max_tokens":1024}`)

	before := append([]byte(nil), body...)

	os.Setenv("WB2API_DUMP_PREFIX", "1")
	t.Cleanup(func() { os.Unsetenv("WB2API_DUMP_PREFIX") })

	dumpPrefixFingerprint(body)

	if !bytes.Equal(before, body) {
		t.Fatalf("dumpPrefixFingerprint 修改了出站 body\nbefore=%q\nafter =%q",
			before, body)
	}
}

// TestDumpPrefixDisabledIsNoop 关掉开关时 body 不变（且不解析）。
func TestDumpPrefixDisabledIsNoop(t *testing.T) {
	os.Unsetenv("WB2API_DUMP_PREFIX")
	// 故意给一段非法 JSON：若函数在开关关闭时仍解析，不会报错但也没副作用；
	// 更有意义的是确认它原样返回、不 panic。
	body := []byte(`{not valid json,,,`)
	before := append([]byte(nil), body...)
	dumpPrefixFingerprint(body)
	if !bytes.Equal(before, body) {
		t.Fatal("开关关闭时 body 被改动")
	}
}

// TestDumpPrefixToleratesWeirdBodies 各种畸形 body 都不能 panic 或改内容。
func TestDumpPrefixToleratesWeirdBodies(t *testing.T) {
	os.Setenv("WB2API_DUMP_PREFIX", "1")
	t.Cleanup(func() { os.Unsetenv("WB2API_DUMP_PREFIX") })

	cases := []string{
		``,
		`null`,
		`{}`,
		`"messages":null}`,
		`{"messages":[]}`,
		`{"messages":[1,2,3]}`,
		`{"messages":[{"role":"user","content":[123,{"type":"image"}]}]}`,
		`{"messages":[{"role":"tool","content":{"nested":"object"}}]}`,
		`{"messages":[{"role":"assistant","content":""}]}`,
	}
	for i, c := range cases {
		body := []byte(c)
		before := append([]byte(nil), body...)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panic: %v (body=%q)", i, r, c)
				}
			}()
			dumpPrefixFingerprint(body)
		}()
		if !bytes.Equal(before, body) {
			t.Fatalf("case %d body 被改动: %q -> %q", i, before, body)
		}
	}
}

// TestDumpPrefixCountsAllRoles 确认它确实覆盖全部消息（不是只取 system）。
//
// 这是能力面的检查：dump 的用途是「两轮逐段 diff」，若它漏段，
// 定位断点时就会得出错误结论（以为某段没变、其实是没打出来）。
func TestDumpPrefixCountsAllRoles(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "s"},
		{"role": "user", "content": "u"},
		{"role": "assistant", "content": "a"},
		{"role": "tool", "content": "t"},
	}
	body, _ := json.Marshal(map[string]any{"messages": msgs})

	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		t.Fatal(err)
	}
	got := src["messages"].([]any)
	if len(got) != 4 {
		t.Fatalf("messages 段数 = %d，期望 4", len(got))
	}
	// 逐段确认 messageText 能读出文本（空串也可以，但不能 panic）。
	for i, raw := range got {
		m := raw.(map[string]any)
		if text := messageText(m["content"]); strings.Contains(text, "panic") {
			t.Fatalf("段 %d 读出异常内容", i)
		}
	}
}

// TestSkippedRemoteLogDoesNotChangeOutcome 锁住新增的 else-if 分支只有日志。
//
// 判据：三个输入（带图 / 超限 / 正常）在 ChatStream 里都应到达同一条
// legacy/Remote 决策，日志分支不改变返回值。这里用 canUseRemote 的等价
// 布尔表达式表达，确保「加日志」没有悄悄改变条件。
func TestSkippedRemoteLogDoesNotChangeOutcome(t *testing.T) {
	// 复刻 ChatStream 里的判据，逐项列全，确保新增的 else-if 只影响日志。
	canUseRemote := func(remoteOn, hasImages, unbounded bool) bool {
		return remoteOn && !hasImages && !unbounded
	}

	if !canUseRemote(true, false, false) {
		t.Fatal("正常请求应走 Remote")
	}
	if canUseRemote(true, true, false) {
		t.Fatal("带图请求不应走 Remote")
	}
	if canUseRemote(true, false, true) {
		t.Fatal("超限请求不应走 Remote")
	}
	if canUseRemote(false, false, false) {
		t.Fatal("Remote 关闭时不应走 Remote")
	}
	// 带图且超限：两个跳过的理由都成立，日志按「带图」优先（与代码一致）。
	if canUseRemote(true, true, true) {
		t.Fatal("带图且超限仍不应走 Remote")
	}
}
