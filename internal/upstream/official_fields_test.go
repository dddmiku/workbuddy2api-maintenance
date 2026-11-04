package upstream

// 2026-11-04：对齐官方客户端每轮必发的 chat 顶层字段。
//
// 官方 6 份抓包（5.8.3 / 5.8.4 / 5.9.3，CN + AI 两域）顶层键集合逐字相同：
//
//	model, messages, tools, temperature, verbosity, max_tokens, stream,
//	stream_options, reasoning_effort, extra_vars, reasoning_summary
//
// 网关此前缺 temperature / verbosity / reasoning_summary / extra_vars 四个。
// 这几条用例锁定「补齐」与「不覆盖调用方显式值」两件事。

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeOfficial(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return obj
}

func TestApplyOfficialChatFieldsFillsMissing(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := ApplyOfficialChatFields(body, "", 0)
	obj := decodeOfficial(t, out)

	if v, ok := obj["temperature"].(float64); !ok || v != 1 {
		t.Errorf("temperature = %v want 1", obj["temperature"])
	}
	if obj["verbosity"] != "high" {
		t.Errorf("verbosity = %v want high", obj["verbosity"])
	}
	if obj["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_summary = %v want auto", obj["reasoning_summary"])
	}
	ev, ok := obj["extra_vars"].(map[string]any)
	if !ok {
		t.Fatalf("extra_vars 类型 = %T", obj["extra_vars"])
	}
	if ev["initiator"] != "user" {
		t.Errorf("extra_vars.initiator = %v want user", ev["initiator"])
	}
	ge, ok := ev["growthEvent"].(string)
	if !ok {
		// 官方把它序列化成字符串塞进来，不是数组——这是重点，别改成数组。
		t.Fatalf("growthEvent 必须是 string，实际 %T", ev["growthEvent"])
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(ge), &arr); err != nil {
		t.Fatalf("growthEvent 不是可解析的 JSON 数组字符串: %v (%q)", err, ge)
	}
	if len(arr) != 1 || arr[0]["eventCode"] != "chat_request_send" {
		t.Errorf("growthEvent 内容 = %v", arr)
	}
	extra := arr[0]["extra"].(map[string]any)
	if extra["requestModelId"] != "deepseek-v4.1-flash" {
		t.Errorf("requestModelId = %v", extra["requestModelId"])
	}
	if extra["mode"] != "craft" {
		t.Errorf("mode = %v want craft", extra["mode"])
	}
}

// TestApplyOfficialChatFieldsKeepsExplicitValues 调用方给的值一律不被覆盖。
func TestApplyOfficialChatFieldsKeepsExplicitValues(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"temperature":0,"verbosity":"low",` +
		`"reasoning_summary":"concise","extra_vars":{"initiator":"system"}}`)
	out := ApplyOfficialChatFields(body, "", 0)
	obj := decodeOfficial(t, out)

	// temperature:0 是合法且有意义的显式值，绝不能被补成 1。
	if v, ok := obj["temperature"].(float64); !ok || v != 0 {
		t.Errorf("显式 temperature=0 被覆盖成 %v", obj["temperature"])
	}
	if obj["verbosity"] != "low" {
		t.Errorf("显式 verbosity 被覆盖成 %v", obj["verbosity"])
	}
	if obj["reasoning_summary"] != "concise" {
		t.Errorf("显式 reasoning_summary 被覆盖成 %v", obj["reasoning_summary"])
	}
	ev := obj["extra_vars"].(map[string]any)
	if ev["initiator"] != "system" {
		t.Errorf("显式 extra_vars 被覆盖成 %v", ev)
	}
}

// TestApplyOfficialChatFieldsIdempotent 二次调用不应再改动（避免多档重试累积）。
func TestApplyOfficialChatFieldsIdempotent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	first := ApplyOfficialChatFields(body, "", 0)
	second := ApplyOfficialChatFields(first, "", 0)
	if string(first) != string(second) {
		t.Fatal("二次调用产生了改动（不幂等）")
	}
}

// TestApplyOfficialChatFieldsBadBody 坏 body 原样返回，不二次错误化。
func TestApplyOfficialChatFieldsBadBody(t *testing.T) {
	for _, bad := range [][]byte{nil, {}, []byte("not json"), []byte("[]")} {
		out := ApplyOfficialChatFields(bad, "", 0)
		if string(out) != string(bad) {
			t.Errorf("坏 body 被改写: %q -> %q", bad, out)
		}
	}
}

func TestDisplayModelName(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4.1-flash": "DeepSeek-V4.1-Flash",
		"glm-5.2":             "GLM-5.2",
		"hy3":                 "HY3",
		"":                    "",
	}
	for in, want := range cases {
		if got := displayModelName(in); got != want {
			t.Errorf("displayModelName(%q) = %q want %q", in, got, want)
		}
	}
}

// TestRandomUUIDv4Shape 形态必须是 8-4-4-4-12 且版本位为 4。
func TestRandomUUIDv4Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := randomUUIDv4()
		if len(id) != 36 {
			t.Fatalf("长度 = %d want 36: %q", len(id), id)
		}
		if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
			t.Fatalf("分隔位置不对: %q", id)
		}
		if id[14] != '4' {
			t.Fatalf("版本位 = %c want 4: %q", id[14], id)
		}
		if seen[id] {
			t.Fatalf("重复 ID: %q", id)
		}
		seen[id] = true
	}
}

// TestOfficialFieldsInOutboundPipeline 端到端：prepareBody 之后四个字段都在。
//
// 这条用例守的是「注入器真的接进了出站链路」——只测 ApplyOfficialChatFields
// 本身无法发现「函数写好了但没人调用」。
func TestOfficialFieldsInOutboundPipeline(t *testing.T) {
	c := New()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out := c.prepareBody(body, "cn", "uid-12345678", "conv-1")
	obj := decodeOfficial(t, out)
	for _, key := range []string{"temperature", "verbosity", "reasoning_summary", "extra_vars"} {
		if _, ok := obj[key]; !ok {
			t.Errorf("出站 body 缺 %s", key)
		}
	}
	// growthEvent 必须是字符串
	ev := obj["extra_vars"].(map[string]any)
	if _, ok := ev["growthEvent"].(string); !ok {
		t.Errorf("growthEvent 类型 = %T want string", ev["growthEvent"])
	}
	// 不该误伤既有字段
	if !strings.Contains(string(out), "prompt_cache_key") {
		t.Error("prompt_cache_key 丢了")
	}
}
