// tooltext.go — 从上游正文/思考里抽取文本协议的工具调用。
//
// ═══ 为什么需要这一环（2026-10-16 实测）═══
//
// Remote 端点不透传 tools 结构（它自带沙箱，见 remote.go 文件头），客户端工具只能
// 以 prompt.go 注入的 opencode_tool_call 文本块表达：
//
//	<opencode_tool_call>{"name":"Read","input":{"file_path":"/etc/hostname"}}</opencode_tool_call>
//
// 旧版**只 strip 不抽取**：stream.go 的 visibleText() 把块从可见文本里删掉，
// 但没有任何地方把它解析成调用。后果是模型发出的工具请求凭空消失——实测
// chat/completions 带 tools + tool_choice=required 时，模型在思考里说
// "So I should just emit the tool call block."，正文里确实发了块，而客户端收到的是
// finish_reason=stop、零 tool_calls、正文为占位文案
// "(trae upstream returned an empty response)"。整条工具链路等于断了。
//
// 参考实现（autumnsentiment/Trae2api-cn 的 cli_client.extract_text_tool_calls）
// 在 strip 的同时解析块并交付调用。本文件补齐这一环。
//
// ═══ 为什么扫「累积原文」而不是每次的增量 ═══
//
// 块会跨事件分片到达（`<opencode_tool_` / `call>{"name":` / …），任何单个增量里都
// 看不到闭合标签。translator 的 textAccumulator 恰好保留了累积快照（raw），
// 在快照上扫描 + 用调用签名去重，块闭合的那一次就能稳定抽出来，且不会重复交付。
//
// ═══ 为什么思考也要扫 ═══
//
// 实测模型常把工具块写在**思考**里（它按提示"这一轮不要写正文"理解，于是只留块
// 在推理轨迹里）。参考实现同样从 reasoning 与 content 两处抽取
// （translate_web_events 的 reasoning_calls + content_calls）。
package trae

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
)

// textToolCall 一个从文本里解析出来的客户端工具调用。
type textToolCall struct {
	// ID 由签名派生：同一调用跨事件重复扫描时保持不变（客户端会把它当 tool_use id 回传）。
	ID string
	// Name 工具名（必须是调用方声明的工具之一，模型负责照抄）。
	Name string
	// Arguments 参数 JSON 字符串（OpenAI 形态；无参数时为 "{}"）。
	Arguments string
	// signature 去重键（name + 规范化参数）。
	signature string
}

// textToolTags 是文本协议的工具块标签——**唯一来源**，剥离与抽取共用。
//
// 2026-10-16 重构：此前剥离用 stream.go 的 toolBlockPairs、抽取用本表，两处各写一份
// 且**已经漂移**（探针实测：`toolcall` 只抽取不剥离、`bash/read/write/edit/glob/grep/task`
// 只剥离不抽取）。注释里那句「两处必须同步增删」靠自觉维持不住，所以改成单一来源 +
// 单一扫描器（scanToolBlocks）：任何一处增删标签，两侧行为自动一致。
//
// 前六个是**客户端工具协议**的序列化形态（opencode_tool_call 是本网关注入的）；
// 后七个是上游沙箱工具或其它客户端历史里出现过的形态——它们**不作为客户端调用交付**
// （客户端没声明这些工具名，交付未声明调用会让整轮失败），但必须参与扫描，
// 否则半截标记会漏进正文。
var textToolTags = []string{
	"opencode_tool_call",
	"tool_use",
	"tool_call",
	"toolcall",
	"tool_cell",
	"invoke",
	"bash",
	"read",
	"write",
	"edit",
	"glob",
	"grep",
	"task",
}

// toolBlock 文本里一个**已闭合**的工具块（含位置，供剥离与抽取共用）。
type toolBlock struct {
	// tag 标签名（小写，不含尖括号）。
	tag string
	// attrs 开标签里除标签名外的属性串（`<tool_call name="Read">` 的 ` name="Read"`）。
	attrs string
	// body 开闭标签之间的内容。
	body string
	// start 开标签 '<' 的下标。
	start int
	// end 闭标签 '>' 之后的下标。
	end int
}

// scanToolBlocks 扫出文本里所有已闭合的工具块，按出现顺序返回。
//
// **未闭合的块不返回**：分片还没到齐，由 holdIncompleteToolBlock 负责压住
// （压住 ≠ 删除，等后续分片到齐后再判定）。
//
// 这是本包**唯一**的文本工具块扫描器：剥离（visibleText）与抽取
// （extractTextToolCalls）都走它，因此两者的标签集合与边界判定不可能不一致。
func scanToolBlocks(text string) []toolBlock {
	if text == "" {
		return nil
	}
	lowered := strings.ToLower(text)
	var blocks []toolBlock
	from := 0
	for from < len(text) {
		tag, at := nextToolTag(lowered, from)
		if at < 0 {
			break
		}
		after := at + 1 + len(tag)
		// 标签名必须在此处结束，否则 `<tool_call` 会命中 `<tool_calls>`。
		if after < len(text) && !isTagBoundary(text[after]) {
			from = after
			continue
		}
		gt := strings.IndexByte(text[at:], '>')
		if gt < 0 {
			break // 开标签本身还没读完
		}
		gt += at
		if gt > 0 && text[gt-1] == '/' {
			from = gt + 1 // 自闭合空标签，不携带调用
			continue
		}
		closeTag := "</" + tag + ">"
		end := strings.Index(lowered[gt+1:], closeTag)
		if end < 0 {
			// 未闭合：等后续分片。同一标签的后续块更不可能闭合，直接换下一个标签
			// （不 break 整个循环——别的标签的块可能已经闭合了）。
			from = gt + 1
			continue
		}
		end += gt + 1
		blocks = append(blocks, toolBlock{
			tag:   tag,
			attrs: text[at+1+len(tag) : gt],
			body:  text[gt+1 : end],
			start: at,
			end:   end + len(closeTag),
		})
		from = end + len(closeTag)
	}
	return blocks
}

// nextToolTag 返回 from 之后**最早出现**的工具标签名与其 '<' 的下标（-1 表示没有）。
func nextToolTag(lowered string, from int) (string, int) {
	best, bestAt := "", -1
	for _, tag := range textToolTags {
		at := strings.Index(lowered[from:], "<"+tag)
		if at < 0 {
			continue
		}
		at += from
		if bestAt < 0 || at < bestAt {
			best, bestAt = tag, at
		}
	}
	return best, bestAt
}

// parseToolBlocks 扫出所有已闭合的块，返回**成功解析成调用**的那些（含位置）。
//
// 「成功解析」的判据与交付完全一致（同一个 parseTextToolCall），所以调用方可以
// 放心用它的结果决定「哪些块该从正文里剥掉」：**只有变成调用的块才剥**。
//
// 为什么这条约束重要（2026-10-16 探针实测）：旧实现按标签无条件删除块，
// 于是「模型写了个读不出来的块」时，那段内容**既没变成调用、也没给用户看**——
// 凭空消失。magpie 的 #2414 记录过同一类问题（它选择把读不出的块当文本发出去），
// 两边结论一致：**宁可让用户看到原始块，也不能静默删除**。
func parseToolBlocks(text string) []toolBlock {
	blocks := scanToolBlocks(text)
	if len(blocks) == 0 {
		return nil
	}
	kept := blocks[:0]
	for _, block := range blocks {
		if _, ok := parseTextToolCall(block.attrs, block.body); ok {
			kept = append(kept, block)
		}
	}
	return kept
}

// extractTextToolCalls 从累积原文里抽取尚未见过的工具调用。
//
// seen 是跨事件持久的签名集合，调用方负责传入同一个 map（同一调用只交付一次）。
func extractTextToolCalls(text string, seen map[string]bool) []textToolCall {
	if text == "" || seen == nil {
		return nil
	}
	var calls []textToolCall
	for _, block := range scanToolBlocks(text) {
		call, ok := parseTextToolCall(block.attrs, block.body)
		if !ok {
			continue
		}
		signature := textToolSignature(call)
		if seen[signature] {
			continue
		}
		seen[signature] = true
		call.signature = signature
		call.ID = "call_" + signature
		calls = append(calls, call)
	}
	return calls
}

// isTagBoundary 报告该字节能否紧跟标签名（`>`、空白、`/`）。
func isTagBoundary(c byte) bool {
	switch c {
	case '>', '/', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// parseTextToolCall 解析一个块的属性与正文，得到调用。
//
// 认三种形态（对齐参考 _parse_text_tool_body）：
//   - JSON 正文：{"name":"Read","input":{...}}（opencode_tool_call 的协议形态）
//   - 属性 + JSON 正文：<tool_call name="Read">{"file_path":"/x"}</tool_call>
//   - 属性 + XML 参数：<invoke name="Read"><param name="file_path">/x</param></invoke>
func parseTextToolCall(attrs, body string) (textToolCall, bool) {
	trimmed := strings.TrimSpace(body)
	attrName := attributeValue(attrs, "name")

	// 形态 1：正文本身就是调用对象。
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) == nil {
			name := firstNonEmptyString(obj, "name", "tool", "tool_name", "toolName")
			if name == "" {
				name = attrName
			}
			if name != "" {
				return textToolCall{Name: name, Arguments: encodeArguments(argumentValue(obj))}, true
			}
		}
	}

	if attrName == "" {
		return textToolCall{}, false
	}
	// 形态 2：属性给名，正文是参数对象。
	if strings.HasPrefix(trimmed, "{") {
		var args map[string]any
		if json.Unmarshal([]byte(trimmed), &args) == nil {
			return textToolCall{Name: attrName, Arguments: encodeArguments(args)}, true
		}
	}
	// 形态 3：XML 参数。
	if args := parseXMLArguments(trimmed); len(args) > 0 {
		return textToolCall{Name: attrName, Arguments: encodeArguments(args)}, true
	}
	// 属性给名、无参数：无参工具是合法调用（参考实现同样接受）。
	return textToolCall{Name: attrName, Arguments: "{}"}, true
}

// argumentValue 从调用对象里取参数（四种拼写）。
func argumentValue(obj map[string]any) any {
	for _, key := range []string{"input", "arguments", "parameters", "args"} {
		if value, present := obj[key]; present {
			switch typed := value.(type) {
			case string:
				// 参数可能是**已序列化的 JSON 字符串**（OpenAI 助手消息形态）。
				trimmed := strings.TrimSpace(typed)
				if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
					return json.RawMessage(trimmed)
				}
				if trimmed == "" {
					return map[string]any{}
				}
				return typed
			case map[string]any:
				return typed
			}
		}
	}
	return map[string]any{}
}

// encodeArguments 把参数压成 JSON 字符串（OpenAI 的 function.arguments 形态）。
func encodeArguments(value any) string {
	switch typed := value.(type) {
	case nil:
		return "{}"
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "{}"
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed
		}
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	case json.RawMessage:
		if json.Valid(typed) {
			return string(typed)
		}
		return "{}"
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

// parseXMLArguments 解析 <param name="x">value</param> 形态的参数。
func parseXMLArguments(text string) map[string]any {
	if text == "" {
		return nil
	}
	args := map[string]any{}
	lowered := strings.ToLower(text)
	from := 0
	for {
		start := strings.Index(lowered[from:], "<param")
		if start < 0 {
			break
		}
		start += from
		gt := strings.IndexByte(text[start:], '>')
		if gt < 0 {
			break
		}
		gt += start
		name := attributeValue(text[start+len("<param"):gt], "name")
		end := strings.Index(lowered[gt+1:], "</param>")
		if end < 0 {
			break
		}
		end += gt + 1
		if name != "" {
			value := strings.TrimSpace(text[gt+1 : end])
			// 参数值可能是 JSON 字面量（数字/布尔/对象），解析失败就当字符串。
			var decoded any
			if json.Unmarshal([]byte(value), &decoded) == nil {
				args[name] = decoded
			} else {
				args[name] = value
			}
		}
		from = end + len("</param>")
	}
	return args
}

// attributeValue 从属性串里取一个属性的值（双引号、单引号、裸值三种都认）。
func attributeValue(attrs, key string) string {
	if attrs == "" || key == "" {
		return ""
	}
	lowered := strings.ToLower(attrs)
	index := strings.Index(lowered, strings.ToLower(key))
	for index >= 0 {
		// 属性名必须独立（避免 name 命中 filename）。
		before := index == 0 || attrs[index-1] == ' ' || attrs[index-1] == '\t' || attrs[index-1] == '\n'
		after := index + len(key)
		if before && after < len(attrs) && (attrs[after] == '=' || attrs[after] == ' ' || attrs[after] == '\t') {
			rest := strings.TrimLeft(attrs[after:], " \t")
			if strings.HasPrefix(rest, "=") {
				rest = strings.TrimLeft(rest[1:], " \t")
				if len(rest) > 0 {
					switch rest[0] {
					case '"', '\'':
						if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
							return rest[1 : 1+end]
						}
					default:
						end := strings.IndexAny(rest, " \t\n>")
						if end < 0 {
							return rest
						}
						return rest[:end]
					}
				}
			}
		}
		next := strings.Index(lowered[after:], strings.ToLower(key))
		if next < 0 {
			break
		}
		index = after + next
	}
	return ""
}

// firstNonEmptyString 取第一个非空字符串字段。
func firstNonEmptyString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// textToolSignature 调用签名（name + 规范化参数），用作去重键与 id 来源。
//
// 规范化只压空白，不改语义——同一调用的两种排版（空格差异）视为同一个，
// 而参数不同就是不同调用（模型连发两次同名同参的调用会被去重，这是刻意的：
// 协议明确要求「不要重复已完成的调用」，重复交付只会让客户端执行两次）。
func textToolSignature(call textToolCall) string {
	normalized := strings.Join(strings.Fields(call.Arguments), " ")
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(call.Name))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(normalized))
	return fmt.Sprintf("%016x", hash.Sum64())
}
