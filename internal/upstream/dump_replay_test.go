package upstream

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDumpedRequestGetsNeutralized 用**生产落盘的真实请求体**验证中和是否命中。
//
// 背景：面板/落盘样本显示网关发出的 system 首行仍是
//
//	"x​-a​nthropic-b​illing-h​eader: cc_version=2.1.301.fb5; ..."
//
// 零宽字符还在——需要判断是「中和没命中」还是「中和命中了但断词位置不同」。
func TestDumpedRequestGetsNeutralized(t *testing.T) {
	dump := os.Getenv("WB_DUMP_FILE")
	if dump == "" {
		t.Skip("未设置 WB_DUMP_FILE（生产落盘样本路径）")
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("读取样本失败: %v", err)
	}
	var record struct {
		Request string `json:"request"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("解析样本失败: %v", err)
	}

	body := []byte(record.Request)
	out, changed := NeutralizeBillingHeaders(body)
	t.Logf("预中和 changed=%v  (原 %d 字节 → 新 %d 字节)", changed, len(body), len(out))

	if !changed {
		t.Fatalf("生产落盘的请求体未被预中和命中 —— 这正是渠道拒绝的直接原因")
	}

	// 提取首行，确认计费头 token 已被打断
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs, _ := obj["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if role, _ := msg["role"].(string); role != "system" {
			continue
		}
		var first string
		switch c := msg["content"].(type) {
		case string:
			first = c
		case []any:
			if len(c) > 0 {
				part, _ := c[0].(map[string]any)
				first, _ = part["text"].(string)
			}
		}
		if idx := strings.IndexByte(first, '\n'); idx >= 0 {
			first = first[:idx]
		}
		t.Logf("中和后首行前 160 字: %q", first[:minInt(160, len(first))])
		if strings.Contains(first, "x-anthropic-billing-header") {
			t.Fatalf("计费头仍以连续形态存在（未被打断）")
		}
		break
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
