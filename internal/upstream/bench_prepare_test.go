// 基准测试：对比 v3.9.3 与 HEAD 的请求准备热路径耗时。
// 用真实 428k 上下文样本，测 prepareBodyWithBudget 的耗时构成。
package upstream

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// loadBenchSample 从生产落盘样本里取一个最大的请求体。
func loadBenchSample(tb testing.TB) []byte {
	tb.Helper()
	paths := []string{
		`C:\Users\dddmiku\Desktop\na\_probe_channel\dumps\latest2.json`,
		`C:\Users\dddmiku\Desktop\na\_probe_channel\dumps\base.json`,
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var dump struct {
			Request string `json:"request"`
		}
		if err := json.Unmarshal(raw, &dump); err != nil || dump.Request == "" {
			continue
		}
		return []byte(dump.Request)
	}
	tb.Skip("没有生产样本")
	return nil
}

// BenchmarkNeutralizeBillingHeaders 只测计费头预中和这一环。
func BenchmarkNeutralizeBillingHeaders(b *testing.B) {
	body := loadBenchSample(b)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NeutralizeBillingHeaders(body)
	}
}

// BenchmarkJSONDecodeOnly 只测纯解码（作为对照基线）。
func BenchmarkJSONDecodeOnly(b *testing.B) {
	body := loadBenchSample(b)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var obj any
		_ = json.Unmarshal(body, &obj)
	}
}

// BenchmarkPrepareBody 测完整的出站准备链。
func BenchmarkPrepareBody(b *testing.B) {
	body := loadBenchSample(b)
	c := &Client{}
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.prepareBodyWithBudget(body, "global", "bench-uid", "conv-1")
	}
}

// TestNeutralizeBillingHeadersActuallyChanges 确认这一环在大样本上到底改没改。
// 若 changed=false，说明它白做了一次全量解码——那是纯浪费。
func TestNeutralizeBillingHeadersActuallyChanges(t *testing.T) {
	body := loadBenchSample(t)
	out, changed := NeutralizeBillingHeaders(body)
	t.Logf("样本 %d 字节，changed=%v，输出 %d 字节", len(body), changed, len(out))
	if !bytes.Equal(out, body) && !changed {
		t.Fatal("输出变了却没报 changed")
	}
}

// BenchmarkNeutralizeBillingHeadersCleanBody 不含任何触发串的 body（绝大多数请求）：
// 优化后应只做一次字节扫描就返回，不再解码。
func BenchmarkNeutralizeBillingHeadersCleanBody(b *testing.B) {
	body := loadBenchSample(b)
	// 造一份干净样本：把已知触发串替换掉，并去掉零宽
	clean := bytes.ReplaceAll(body, []byte(wafBreakMarker), nil)
	for _, tok := range channelFirstLineTriggerTokens {
		clean = bytes.ReplaceAll(clean, []byte(tok), []byte("redacted"))
	}
	for _, sentence := range channelTriggerSentences {
		clean = bytes.ReplaceAll(clean, []byte(sentence), []byte("redacted"))
	}
	b.SetBytes(int64(len(clean)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, changed := NeutralizeBillingHeaders(clean); changed {
			b.Fatal("clean body must not change")
		}
	}
}

// BenchmarkEnsureConsoleSystemFastPath 首条已是 system 时的快速路径。
func BenchmarkEnsureConsoleSystemFastPath(b *testing.B) {
	body := loadBenchSample(b)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ensureConsoleSystem(body)
	}
}

// BenchmarkMessagesStartWithSystem 纯字节扫描本身有多快。
func BenchmarkMessagesStartWithSystem(b *testing.B) {
	body := loadBenchSample(b)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = messagesStartWithSystem(body)
	}
}
