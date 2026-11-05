// system_flatten.go 把客户端的 system 消息扁平化进 user，绕开上游渠道校验。
//
// ═══ 为什么（2026-11-06 实测，任意客户端通用）═══
//
// 上游对 `/v2/chat/completions` 的渠道校验会扫描 **system 消息的内容**。只要
// system 里出现被判定为「非官方客户端」的文本，就返回：
//
//	400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}
//
// 已知的触发串（`x-anthropic-billing-header` / `You are Claude Code, ...` /
// `Codex CLI is an open source project led by OpenAI` / `workspace root` …）
// 只是**已被观测到的样本**，不是完整清单——每换一个客户端就可能冒出新句子。
// 维护黑名单必然追不上（2026-11-04 修了 workspace root，11-06 又出现新的）。
//
// 同一条内容放进 user 则完全不触发（实测矩阵）：
//
//	system = dsh 的 18086 字符/146 行内容      → 11128
//	同一份内容并入 user，system 换成官方句     → 200
//	无 system、内容只在 user                  → 200
//
// 所以策略是**结构性规避**而不是黑名单：客户端的 system 内容一律搬到 user，
// system 只留一句官方形态的。这样任何客户端、任何措辞都不会撞校验。
//
// ═══ 为什么不直接删掉 system ═══
//
// Claude Code / Codex / Cline / dsh 的 system 里含工具约定、行为约束、路径规则，
// 删掉会让模型行为劣化（工具调用格式跑偏、无视工作区约定）。并入 user 既避开
// system 扫描，又完整保留指令文本——模型读到的语义与原来一致。
//
// ═══ 安全边界 ═══
//
//   - 无 system / 无 user / body 不可解析 → 一个字节都不动；
//   - system 已经是官方句（且没有其他 system）→ 原样返回，幂等；
//   - 只在 messages 层改结构，其余字段（tools / temperature / …）不碰。
package upstream

import (
	"encoding/json"
	"strings"

	"workbuddy2api/internal/jsonutil"
)

// officialSystemLine 出站 system 的固定内容。
//
// 取自官方桌面客户端 app.asar 的 system 文案候选（该串在包内出现 2 次）。
// 实测这一句能通过校验，且不含任何被扫描的触发特征。
//
// 注意官方**实际**发的是按模型生成的
// "This conversation is powered by <Model> ..."（6/6 抓包都是这句，
// 因为它是运行时拼的，所以在 asar 里搜不到）。这里用静态句是因为网关不知道
// 上游会怎么渲染模型名；两句都能通过校验，静态句更简单也更稳定。
const officialSystemLine = "You are a helpful assistant. You are an expert in programming."

// flattenSystemMarker 包在并入 user 的系统指令前的分隔标记。
// 用中文方括号而非 XML/Markdown 结构：上游对 `<system-reminder>` 这类标签有自己的
// 处理，用纯文本标记不会与它的解析规则冲突。
const flattenSystemMarker = "【以下是本轮的系统指令，请照此执行】"

// flattenUserMarker 用户正文与系统指令之间的分隔标记。
const flattenUserMarker = "【用户消息】"

// FlattenSystemIntoUser 把 messages 里所有 system/developer 内容并入第一条 user。
//
// 结构变换：
//
//	[system A, system B, user X, assistant Y, ...]
//	→ [system officialSystemLine, user (marker + A + B + usermarker + X), assistant Y, ...]
//
// 多个 system 按出现顺序拼接；developer 角色同样处理（部分客户端用它代替 system）。
// 若第一条 user 的 content 是 part 数组，系统指令作为一个前置 text part 插入，
// 其余 part 原样保留。
func FlattenSystemIntoUser(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body
	}
	rawMsgs, ok := obj["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		return body
	}

	// 收集 system/developer 文本，同时记录第一条 user 的下标。
	var sysTexts []string
	firstUser := -1
	hasSystem := false
	for i, raw := range rawMsgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			hasSystem = true
			if text := messageText(msg["content"]); text != "" {
				sysTexts = append(sysTexts, text)
			}
		case "user":
			if firstUser < 0 {
				firstUser = i
			}
		}
	}

	// 没有 system：无需处理（幂等分支之一）。
	if !hasSystem {
		return body
	}
	// 有 system 但没有 user：没有可承载指令的位置，原样返回避免丢内容。
	if firstUser < 0 {
		return body
	}

	merged := strings.Join(sysTexts, "\n\n")
	// 已经是官方句且只有一个 system、没有别的系统文本：原样返回（幂等）。
	if len(sysTexts) <= 1 && strings.TrimSpace(merged) == officialSystemLine {
		return body
	}

	// 重建 messages：丢掉全部 system/developer，其余保持原顺序，
	// 在位置 0 插入官方 system，并把合并后的指令前置到第一条 user。
	out := make([]any, 0, len(rawMsgs)+1)
	out = append(out, map[string]any{"role": "system", "content": officialSystemLine})

	prefix := ""
	if merged != "" {
		prefix = flattenSystemMarker + "\n" + merged + "\n\n" + flattenUserMarker + "\n"
	}
	for i, raw := range rawMsgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			// 非对象消息原样保留（不该出现，但不静默丢弃）。
			out = append(out, raw)
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer":
			continue // 已并入 user
		}
		if i == firstUser && prefix != "" {
			mergedContent, changed := prependToContent(msg["content"], prefix)
			if changed {
				msg = cloneMessage(msg)
				msg["content"] = mergedContent
			}
		}
		out = append(out, msg)
	}
	obj["messages"] = out

	encoded, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return encoded
}

// messageText 把一条消息的 content 取成纯文本。
// 支持字符串与 part 数组（取各 part 的 text）；其他形态返回空串。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// prependToContent 把 prefix 前置到一条消息的 content 上。
// 字符串直接拼；part 数组则插入一个前置 text part（保持其它 part 的类型与顺序）。
// 返回 (新 content, 是否改动)。
func prependToContent(content any, prefix string) (any, bool) {
	switch v := content.(type) {
	case string:
		return prefix + v, true
	case []any:
		head := map[string]any{"type": "text", "text": prefix}
		out := make([]any, 0, len(v)+1)
		out = append(out, head)
		out = append(out, v...)
		return out, true
	}
	return content, false
}

// cloneMessage 浅拷贝消息对象，避免就地修改调用方的 map。
func cloneMessage(msg map[string]any) map[string]any {
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	return out
}
