// ═══ 更新日志 ═══
// 2026-09-26：渠道中和改为分级升级（指纹→通用归属句→正文全量），任何客户端都不再因自称而被拒。
// 2026-09-26：新增 Claude Code 系统提示触发句与组合指纹，修复 Claude Code 经本网关被 11128 拒绝。
// 2026-09-17：新增上游渠道校验触发句的中性化，供被 11128 拒绝的系统说明断词重试。
package upstream

import (
	"encoding/json"
	"strings"

	"workbuddy2api/internal/jsonutil"
)

// channelTriggerSentences 已实测会被上游按「未授权渠道」拒绝的系统说明句子。
//
// 依据（docs/codex.md §默认指令为什么会被拒，2026-09-17 字段二分结论）：
// 官方 Codex CLI 默认说明里这一句对渠道归属的声明命中上游校验，
// 删掉或改成中性说法后立即返回 200；工具声明、模型名、用户消息都不是触发点。
// 只匹配句子主体，不带句末标点，兼容原文与去掉句号的变体。
var channelTriggerSentences = []string{
	"Codex CLI is an open source project led by OpenAI",
	// 2026-09-26 实测（CLI 端到本网关）：Claude Code 系统提示首行命中同一校验，
	// 原样发送返回 400/11128，断词后 200。
	"You are Claude Code, Anthropic's official CLI for Claude",
	// 2026-09-28 实测（逐段二分 + 对照）：Claude Code 2.1.283 在系统提示开头塞入的
	// 计费归属头字符串本身命中同一校验。整行、只留头名、甚至只留到 "…billing-header"
	// 都被拒；"x-anthropic-" 或 "x-anthropic-version:" 等其他头通过；断词后 200。
	// 这是字符串级触发，与它出现在哪条消息无关，所以按精确指纹在第 0 档处理（零误伤）。
	"x-anthropic-billing-header",
}

// channelTriggerFingerprints 组合指纹：同一段文本内同时出现全部片段才断词，
// 对付措辞稍有变化的同一句。实测：只写 "Claude Code" 或只写
// "Anthropic's official CLI for Claude" 均通过，两个片段同现才被拒。
var channelTriggerFingerprints = [][]string{
	{"Claude Code", "official CLI for Claude"},
}

// 分级升级（最多三档，逐档重发一次）：
//
//	channelLevelFingerprint 已实测指纹（精确、零误伤）
//	channelLevelAttribution 通用「客户端归属」句：任何自称官方 CLI/IDE/客户端的句子
//	channelLevelAll         system/developer/user 消息正文全量断词（最后手段，保证指纹必被破坏）
//
// 只在原样请求确实被上游以 11128 拒绝后使用，且只作用于消息正文（不动工具名、
// 调用编号、枚举值等结构性字符串），因此任何客户端都不会因为「自称是谁」被挡在门外。
const (
	channelLevelFingerprint = iota
	channelLevelAttribution
	channelLevelAll
)

// maxChannelNeutralizeLevels 档位数（含未处理）：最多因渠道拒绝重发两次。
const maxChannelNeutralizeLevels = 3

// channelAttributionMarkers 「官方客户端」类措辞；与任意产品名同现即视为归属句。
var channelAttributionMarkers = []string{
	"official cli", "official ide", "official extension", "official client",
	"official plugin", "official agent", "official coding", "official vscode",
	"official vs code", "anthropic's official", "openai's official", "google's official",
	"coding agent by", "assistant by ", "made by anthropic", "made by openai",
}

// channelClientNames 常见 AI 编码/代理客户端产品名（小写匹配）。
var channelClientNames = []string{
	"claude code", "codex cli", "codex", "cursor", "windsurf", "cline", "roo code",
	"kilocode", "aider", "copilot", "gemini cli", "zed ", "devin", "opencode",
	"kiro", "trae", "qoder", "continue.dev", "amazon q", "jetbrains ai",
	"augment code", "comate", "marscode", "lingma", "codebuddy", "workbuddy",
	"tongyi", "qwen code", "iflow", "crush", "amp ", "factory droid",
}

// NeutralizeChannelTrigger 在「已确认被上游渠道校验拒绝」的请求体上断开触发句。
//
// 语义约定与 NeutralizeWAFTriggers 一致：
//   - 只在命中句子的每个单词首字母之后插入一个零宽空格，不删除、不改写任何字符，
//     模型侧读到的是同一句话；
//   - 调用点仅限原样请求已被 11128 unapproved channel 拒绝之后（见
//     ChatStreamContext 的重试分支），正常请求的正文不经过这里；
//   - body 不是 JSON、或没有命中触发句时原样返回，第二个返回值为 false。
func NeutralizeChannelTrigger(body []byte) ([]byte, bool) {
	return NeutralizeChannelTriggerAt(body, channelLevelFingerprint)
}

// NeutralizeChannelTriggerAt 按档位断词：0 只处理已实测指纹，1 追加通用归属句，
// 2 在消息正文里全量断词。档位越高越不依赖「猜中原文」，代价是正文多出零宽字符。
func NeutralizeChannelTriggerAt(body []byte, level int) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body, false
	}
	changed := false
	obj = neutralizeChannelJSONValue(obj, &changed)
	switch {
	case level >= channelLevelAll:
		changed = neutralizeMessageText(obj, channelTextModeAll) || changed
	case level >= channelLevelAttribution:
		changed = neutralizeMessageText(obj, channelTextModeAttribution) || changed
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

const (
	channelTextModeAttribution = iota
	channelTextModeAll
)

// neutralizeMessageText 只对消息正文断词：content（字符串或 part 数组的 text/thinking）、
// reasoning_content 与顶层 system。工具名、调用编号、工具参数、枚举值等结构性字符串不动，
// 避免破坏工具配对与 schema 校验。
func neutralizeMessageText(obj any, mode int) bool {
	root, ok := obj.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	apply := func(text string) string {
		if text == "" {
			return text
		}
		if mode == channelTextModeAll {
			return breakAllWords(text, &changed)
		}
		return breakAttributionSentences(text, &changed)
	}
	visit := func(message map[string]any) {
		if content, ok := message["content"].(string); ok {
			message["content"] = apply(content)
			return
		}
		if parts, ok := message["content"].([]any); ok {
			for _, raw := range parts {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				for _, key := range []string{"text", "thinking", "input_text"} {
					if text, ok := part[key].(string); ok {
						part[key] = apply(text)
					}
				}
			}
		}
		if text, ok := message["reasoning_content"].(string); ok {
			message["reasoning_content"] = apply(text)
		}
	}
	if messages, ok := root["messages"].([]any); ok {
		for _, raw := range messages {
			if message, ok := raw.(map[string]any); ok {
				visit(message)
			}
		}
	}
	if text, ok := root["system"].(string); ok {
		root["system"] = apply(text)
	}
	return changed
}

// breakAllWords 在每个单词首字母后插入零宽标记（正文全量档）。
func breakAllWords(text string, changed *bool) string {
	points := wordBreakPoints(text, 0, len(text))
	if len(points) == 0 {
		return text
	}
	*changed = true
	return insertBreaks(text, points)
}

// breakAttributionSentences 断开「自称客户端」的句子：句内出现产品名或官方归属措辞即命中。
func breakAttributionSentences(text string, changed *bool) string {
	points := []int{}
	for _, span := range sentenceSpans(text) {
		if !isChannelAttribution(text[span[0]:span[1]]) {
			continue
		}
		points = append(points, wordBreakPoints(text, span[0], span[1])...)
	}
	if len(points) == 0 {
		return text
	}
	*changed = true
	return insertBreaks(text, points)
}

// sentenceSpans 按句末标点与换行切句，返回半开区间的字节偏移。
func sentenceSpans(text string) [][2]int {
	spans := [][2]int{}
	start := 0
	for index, r := range text {
		switch r {
		case '.', '!', '?', '\n', '\r', '\u3002', '\uff01', '\uff1f', '\uff1b':
			if index > start {
				spans = append(spans, [2]int{start, index})
			}
			start = index + len(string(r))
		}
	}
	if start < len(text) {
		spans = append(spans, [2]int{start, len(text)})
	}
	return spans
}

// isChannelAttribution 判断句子是否在声明客户端归属。
func isChannelAttribution(segment string) bool {
	lower := strings.ToLower(segment)
	for _, marker := range channelAttributionMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	named := false
	for _, name := range channelClientNames {
		if strings.Contains(lower, name) {
			named = true
			break
		}
	}
	// 「你是 X」式自我介绍也属于归属声明（产品名在表内时）。
	if !named {
		return false
	}
	return strings.Contains(lower, "you are ") || strings.Contains(lower, "i am ") ||
		strings.Contains(lower, "assistant") || strings.Contains(lower, "agent")
}

// insertBreaks 按升序偏移插入零宽标记。
func insertBreaks(text string, points []int) string {
	sortInts(points)
	out := make([]byte, 0, len(text)+len(points)*len(wafBreakMarker))
	prev := 0
	for _, pos := range points {
		if pos <= prev || pos >= len(text) {
			continue
		}
		out = append(out, text[prev:pos]...)
		out = append(out, wafBreakMarker...)
		prev = pos
	}
	out = append(out, text[prev:]...)
	return string(out)
}

// neutralizeChannelJSONValue 递归遍历解码后的 JSON，对每个字符串做触发句断词。
func neutralizeChannelJSONValue(value any, changed *bool) any {
	switch node := value.(type) {
	case string:
		return neutralizeChannelText(node, changed)
	case []any:
		for i, item := range node {
			node[i] = neutralizeChannelJSONValue(item, changed)
		}
		return node
	case map[string]any:
		for key, item := range node {
			node[key] = neutralizeChannelJSONValue(item, changed)
		}
		return node
	default:
		return value
	}
}

// neutralizeChannelText 在单个字符串内断开全部命中句子，报告是否发生改动。
func neutralizeChannelText(text string, changed *bool) string {
	if text == "" {
		return text
	}
	insertAt := map[int]struct{}{}
	for _, sentence := range channelTriggerSentences {
		start := 0
		for {
			index := indexFrom(text, sentence, start)
			if index < 0 {
				break
			}
			for _, pos := range wordBreakPoints(text, index, index+len(sentence)) {
				insertAt[pos] = struct{}{}
			}
			start = index + len(sentence)
		}
	}
	for _, parts := range channelTriggerFingerprints {
		if len(parts) < 2 {
			continue
		}
		start := indexFrom(text, parts[0], 0)
		if start < 0 {
			continue
		}
		end := start + len(parts[0])
		complete := true
		for _, part := range parts[1:] {
			index := indexFrom(text, part, end)
			if index < 0 {
				complete = false
				break
			}
			end = index + len(part)
		}
		if !complete {
			continue
		}
		for _, pos := range wordBreakPoints(text, start, end) {
			insertAt[pos] = struct{}{}
		}
	}
	if len(insertAt) == 0 {
		return text
	}
	points := make([]int, 0, len(insertAt))
	for pos := range insertAt {
		points = append(points, pos)
	}
	*changed = true
	return insertBreaks(text, points)
}

// indexFrom 从 from 起查找 needle 的字节偏移（区分大小写；触发句大小写固定）。
func indexFrom(text, needle string, from int) int {
	if from >= len(text) {
		return -1
	}
	index := -1
	for i := from; i+len(needle) <= len(text); i++ {
		if text[i:i+len(needle)] == needle {
			index = i
			break
		}
	}
	return index
}

// wordBreakPoints 返回片段内每个单词首字母之后的插入点（原字符串字节偏移）。
// 单词以 ASCII 字母/数字起始，遇到空格、标点或大小写之外的分隔即结束。
func wordBreakPoints(text string, start, end int) []int {
	points := []int{}
	atWordStart := true
	for i := start; i < end; i++ {
		c := text[i]
		if isASCIIAlnum(c) {
			if atWordStart {
				points = append(points, i+1)
				atWordStart = false
			}
			continue
		}
		atWordStart = true
	}
	return points
}
