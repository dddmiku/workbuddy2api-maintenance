package upstream

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNeutralizeRealProductionDumpIsIdempotent 用**生产落盘的真实请求**验证幂等。
//
// 样本来源：网关渠道拒绝转储（ChannelDumpDir），2026-10-17 04:49 抓到的
// global 域请求，其 system 首行含 36 个零宽字符、每词首嵌套 5 层：
//
//	'x[ZW×5]-a[ZW×5]nthropic-b[ZW×5]illing-h[ZW×5]eader: c[ZW]c_v[ZW]ersion=2[ZW].1[ZW].3[ZW]01.4[ZW]4f; ...'
//
// 这正是「多档重试反复断词、零宽累积」的现场证据。测试要求：
// 把这份**已经被污染**的 body 再过一遍中和，输出必须是规范形态（每词首恰好 1 个零宽），
// 且再过一次不再变化。
//
// 样本不在仓库里（含真实账号数据），所以目录缺失时跳过——CI 上不红，
// 本地 `_probe_channel/dumps` 存在时会真跑。
func TestNeutralizeRealProductionDumpIsIdempotent(t *testing.T) {
	dirs := []string{
		filepath.Join("..", "..", "..", "..", "_probe_channel", "dumps"),
		`C:\Users\dddmiku\Desktop\na\_probe_channel\dumps`,
	}
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(files) == 0 {
		t.Skip("生产转储样本目录不存在，跳过（含真实账号数据，不入库）")
	}

	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// 转储格式：{\"status\":..., \"request\":\"<原始 body 字符串>\"}
		var dump struct {
			Request string `json:"request"`
		}
		if err := json.Unmarshal(raw, &dump); err != nil || dump.Request == "" {
			// 也可能是直接存的 body
			if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
				continue
			}
			dump.Request = string(raw)
		}
		body := []byte(dump.Request)
		if !bytes.Contains(body, []byte(`"messages"`)) {
			continue
		}
		// 只挑首行真含触发串的样本（绝大多数请求没有）。
		if !bytes.Contains(body, []byte("nthropic")) && !bytes.Contains(body, []byte("billing")) {
			continue
		}

		first, changed := NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
		second, changedAgain := NeutralizeChannelTriggerAt(first, channelLevelFingerprint)

		if !bytes.Equal(first, second) || changedAgain {
			t.Fatalf("%s: 第 0 档不幂等（changed=%v 再跑 changed=%v）\nfirst =%q\nsecond=%q",
				filepath.Base(path), changed, changedAgain, snippet(first), snippet(second))
		}
		// 只检查 system **首行**——那是中和器唯一改写的区域。
		// 正文里可能存在客户端自带的零宽字符（用户自己的 CLAUDE.md 里就有，
		// 实测 base.json 第 122 行 `~/.c[ZW][ZW]o[ZW]dex/AGENTS.md`），
		// 那不是中和器产生的，也不在本次作用域内。
		head := firstSystemFirstLine(t, first)
		if run := maxZeroWidthRun(head); run > 1 {
			t.Fatalf("%s: 规范化后首行仍有 %d 个连续零宽字符（应为 1）：%q",
				filepath.Base(path), run, head)
		}
		// 触发串必须已被打断。
		for _, tok := range channelFirstLineTriggerTokens {
			if bytes.Contains(first, []byte(tok)) {
				t.Fatalf("%s: 触发串 %q 仍是连续形态（断词失效）", filepath.Base(path), tok)
			}
		}
		checked++
		t.Logf("%s: 首行零宽峰值=%d changed=%v", filepath.Base(path), maxZeroWidthRun(head), changed)
	}
	if checked == 0 {
		t.Skip("样本目录里没有命中触发串的请求")
	}
	t.Logf("已用 %d 份生产样本验证幂等", checked)
}

// firstSystemFirstLine 取出 body 里第一条 system 消息的首行。
func firstSystemFirstLine(t *testing.T, body []byte) string {
	t.Helper()
	var obj any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	root, _ := obj.(map[string]any)
	messages, _ := root["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "system") {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			head, _ := splitFirstLine(content)
			return head
		case []any:
			for _, rawPart := range content {
				part, _ := rawPart.(map[string]any)
				if text, ok := part["text"].(string); ok {
					head, _ := splitFirstLine(text)
					return head
				}
			}
		}
	}
	return ""
}
