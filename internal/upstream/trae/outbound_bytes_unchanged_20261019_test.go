package trae

// 2026-10-19 决定性对照：改动前后，出站 body 必须逐字节相同。
//
// 用户问「会影响模型能力吗，会丢失信息吗」。上面那组测试证明了 dump 函数
// 自身无副作用，但那是「单元级」的证据。这里做「链路级」的证据：
//
//  1. 用同一份 prepared 请求体；
//  2. 走 BuildChatBodyWithCatalog（真正决定出站内容的函数）；
//  3. 分别算 SHA-256，必须相同。
//
// 只要这一步相同，就说明网关送到上游的每一个字节都没变——
// 模型看到的内容完全一致，能力不受影响、信息不丢失。
//
// 注意：这里刻意不比较「日志文本」，因为新增的就是日志，它本来就会有差异。
// 要比的是**出站 payload**。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// TestOutboundBodyBytesUnchanged 出站 payload 的哈希不因 dump 开关而变。
func TestOutboundBodyBytesUnchanged(t *testing.T) {
	prepared := []byte(`{
		"model":"trae:deepseek-v4.1-flash",
		"messages":[
			{"role":"system","content":[{"type":"text","text":"你是 Trae Code"}]},
			{"role":"user","content":"读一下 main.go"},
			{"role":"assistant","content":[{"type":"text","text":"好的"}],
			 "tool_calls":[{"index":0,"id":"call_a","type":"function",
			   "function_call":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},
			{"role":"tool","tool_call_id":"call_a","name":"read_file",
			 "content":"package main\nfunc main(){}"}
		],
		"tools":[{"type":"function","function":{"name":"read_file",
			"description":"读取文件",
			"parameters":"{\"type\":\"object\",\"properties\":{\"path\":{\"type\":\"string\"}}}"}}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"max_tokens":4096
	}`)

	sum := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}

	// 开关关闭
	os.Unsetenv("WB2API_DUMP_PREFIX")
	off1, err := BuildChatBodyWithCatalog(prepared, "req-fixed-id", nil)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	off2, err := BuildChatBodyWithCatalog(prepared, "req-fixed-id", nil)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	// 开关打开
	os.Setenv("WB2API_DUMP_PREFIX", "1")
	t.Cleanup(func() { os.Unsetenv("WB2API_DUMP_PREFIX") })
	on1, err := BuildChatBodyWithCatalog(prepared, "req-fixed-id", nil)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	dumpPrefixFingerprint(on1)
	on2, err := BuildChatBodyWithCatalog(prepared, "req-fixed-id", nil)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	if sum(off1) != sum(off2) {
		t.Fatalf("同一输入两次构建结果不一致（非确定性）:\n%s\n%s", sum(off1), sum(off2))
	}
	if sum(off1) != sum(on1) {
		t.Fatalf("开关打开后出站 payload 变了\n关闭: %s\n打开: %s", sum(off1), sum(on1))
	}
	if sum(on1) != sum(on2) {
		t.Fatalf("dump 调用影响了后续构建\n前: %s\n后: %s", sum(on1), sum(on2))
	}

	// 语义级确认：关键字段一个都没丢。
	var out map[string]any
	if err := json.Unmarshal(on1, &out); err != nil {
		t.Fatalf("出站 body 不是合法 JSON: %v", err)
	}
	for _, key := range []string{"messages", "tools", "tool_choice",
		"parallel_tool_calls", "max_tokens", "function", "config_name", "model"} {
		if _, ok := out[key]; !ok {
			t.Errorf("出站 body 丢了字段 %q", key)
		}
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("消息条数 = %d，期望 4", len(msgs))
	}
	// tool 消息的 tool_call_id 必须保留（丢了上游会判孤儿结果）
	last, _ := msgs[3].(map[string]any)
	if last["tool_call_id"] != "call_a" {
		t.Errorf("tool 消息的 tool_call_id 丢了: %v", last["tool_call_id"])
	}
	// assistant 的 function_call 必须还在
	asst, _ := msgs[2].(map[string]any)
	if asst["tool_calls"] == nil {
		t.Error("assistant 的 tool_calls 丢了")
	}
}

// TestPrefixDumpIsReadOnlyOnRealBody 对真实规模的 body 做一次调用，确认内容不变。
//
// 上面用的是小 body；真实请求有几十万 token。这里放大到 2000 段，
// 确认大输入下也逐字节不变（哈希相同即证明）。
func TestPrefixDumpIsReadOnlyOnRealBody(t *testing.T) {
	big := make([]map[string]any, 0, 2000)
	for i := 0; i < 2000; i++ {
		role := "user"
		if i%2 == 0 {
			role = "system"
		}
		big = append(big, map[string]any{
			"role":    role,
			"content": "这是一段用于规模测试的内容块，编号 " + string(rune('A'+i%26)),
		})
	}
	body, err := json.Marshal(map[string]any{"messages": big})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), body...)

	os.Setenv("WB2API_DUMP_PREFIX", "1")
	t.Cleanup(func() { os.Unsetenv("WB2API_DUMP_PREFIX") })
	dumpPrefixFingerprint(body)

	if string(before) != string(body) {
		t.Fatalf("大 body 被改动：len %d -> %d", len(before), len(body))
	}
}
