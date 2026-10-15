// regressions_20261016_test.go — 2026-10-16 三个实测缺陷的契约测试。
//
// 背景（用户报障 + 实测复现）：
//  1. Trae 的思考在 Claude Code 里被渲染成十几个「Thought for 1s」碎片，
//     中间夹着几个字的正文；
//  2. glm-5.3 本身能读图，但带图请求在网关里变成 "[image omitted]" 文本；
//  3. 带 tools 的请求里模型按协议发出的 opencode_tool_call 块被 strip 掉却
//     从未转成 tool_calls——客户端收到 finish_reason=stop、零工具调用、
//     正文为占位文案，整条工具链路等于断了。
package trae

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// reasoningFrames 按顺序取出所有 reasoning_content 增量。
func reasoningFrames(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	for _, frame := range frames(t, sse) {
		for _, raw := range frame["choices"].([]any) {
			delta, _ := raw.(map[string]any)["delta"].(map[string]any)
			if text, ok := delta["reasoning_content"].(string); ok && text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// deltaOrder 返回 delta 字段的出现顺序（只记 reasoning_content / content / tool_calls）。
func deltaOrder(t *testing.T, sse string) []string {
	t.Helper()
	var out []string
	for _, frame := range frames(t, sse) {
		for _, raw := range frame["choices"].([]any) {
			delta, _ := raw.(map[string]any)["delta"].(map[string]any)
			switch {
			case delta["reasoning_content"] != nil:
				out = append(out, "R")
			case delta["content"] != nil:
				out = append(out, "C")
			case delta["tool_calls"] != nil:
				out = append(out, "T")
			}
		}
	}
	return out
}

// TestReasoningDoesNotAlternateWithContent 思考不得与正文交替成帧。
//
// 这是「Claude Code 里思考被切成很多段」的直接判据：上游每个 plan_item 都同时带
// thought 与正文快照，旧实现逐个「先 reasoning 后 content」，于是序列是 R,C,R,C…；
// Anthropic 侧每遇一次 thinking→text→thinking 都要新开一个 content block，
// 客户端就渲染成多个「Thought for Ns」。修好后正文之前的思考合并成**一个** R，
// 正文之后再出现的思考进尾部通道（最多再一个 R）。
func TestReasoningDoesNotAlternateWithContent(t *testing.T) {
	// 上游发的是**累计快照**（每个 plan_item 的 thought/content 都是从头累计的
	// 完整文本），所以每一帧都是前一帧的前缀扩展——这是实测形态。
	upstream := strings.Join([]string{
		"event: output\ndata: {\"reasoning_content\":\"first thought\",\"response\":\"part one\"}\n\n",
		"event: output\ndata: {\"reasoning_content\":\"first thought and second\",\"response\":\"part one and two\"}\n\n",
		"event: output\ndata: {\"reasoning_content\":\"first thought and second and third\",\"response\":\"part one and two and three\"}\n\n",
		"event: done\ndata: {}\n\n",
	}, "")
	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	order := deltaOrder(t, sse)
	// 判据：不得有 R 被「夹在两个 C 之间」——那正是交替成块的形态
	// （R,C,R,C…）。末尾那个 R 是合法的：正文开始后才到达的思考统一在收尾时
	// 合并发出，它后面没有正文了。
	for index, kind := range order {
		if kind != "R" {
			continue
		}
		var before, after bool
		for _, earlier := range order[:index] {
			if earlier == "C" {
				before = true
			}
		}
		for _, later := range order[index+1:] {
			if later == "C" {
				after = true
			}
		}
		if before && after {
			t.Fatalf("思考被夹在正文中间（碎片化的根因）：%v", order)
		}
	}
	if runs := countRuns(order, "R"); runs > 2 {
		t.Fatalf("思考被切成 %d 段，应不超过 2 段（正文前 + 正文后）：%v", runs, order)
	}
	// 内容不能丢：累计快照的最终值要在。
	joined := strings.Join(reasoningFrames(t, sse), "")
	for _, want := range []string{"first thought", "second", "third"} {
		if !strings.Contains(joined, want) {
			t.Errorf("思考内容丢失 %q：%q", want, joined)
		}
	}
	// 正文也要完整。
	content := contentOf(t, sse)
	for _, want := range []string{"part one", "and two", "and three"} {
		if !strings.Contains(content, want) {
			t.Errorf("正文丢失 %q：%q", want, content)
		}
	}
}

// countRuns 数出相邻同类元素形成的段数。
func countRuns(items []string, kind string) int {
	runs, previous := 0, false
	for _, item := range items {
		if item == kind {
			if !previous {
				runs++
			}
			previous = true
			continue
		}
		previous = false
	}
	return runs
}

// TestTextToolCallExtractedFromContent 正文里的 opencode_tool_call 必须转成 tool_calls。
//
// 这是「工具调用凭空消失」的直接判据。旧实现只在 visibleText 里 strip 掉块，
// 没有任何抽取，于是模型发出的调用既不在正文里也不在 tool_calls 里。
func TestTextToolCallExtractedFromContent(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"<opencode_tool_call>{\\\"name\\\":\\\"Read\\\",\\\"input\\\":{\\\"file_path\\\":\\\"/etc/hostname\\\"}}</opencode_tool_call>\"}\n\n" +
		"event: done\ndata: {}\n\n"
	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(sse, `"tool_calls"`) {
		t.Fatalf("文本协议的工具调用没有被抽取成 tool_calls：%q", sse)
	}
	if !strings.Contains(sse, `"name":"Read"`) {
		t.Errorf("工具名丢失：%q", sse)
	}
	if !strings.Contains(sse, `\/etc\/hostname`) && !strings.Contains(sse, `/etc/hostname`) {
		t.Errorf("工具参数丢失：%q", sse)
	}
	// 终态必须是 tool_calls，而不是 stop。
	if !strings.Contains(sse, `"finish_reason":"tool_calls"`) {
		t.Errorf("有工具调用时终态应为 tool_calls：%q", sse)
	}
	// 工具块不得漏进可见正文。
	if strings.Contains(contentOf(t, sse), "opencode_tool_call") {
		t.Errorf("工具块漏进正文：%q", contentOf(t, sse))
	}
	// 不得再交付「上游返回空响应」的占位文案。
	if strings.Contains(contentOf(t, sse), "trae upstream returned an empty response") {
		t.Errorf("占位文案说明抽取没生效：%q", sse)
	}
}

// TestTextToolCallExtractedFromReasoning 思考里的工具块同样要抽取。
//
// 实测模型常把块只写在思考里（它按「这一轮不要写正文」理解提示）。
func TestTextToolCallExtractedFromReasoning(t *testing.T) {
	upstream := "event: output\ndata: {\"reasoning_content\":\"<opencode_tool_call>{\\\"name\\\":\\\"Bash\\\",\\\"input\\\":{\\\"command\\\":\\\"ls\\\"}}</opencode_tool_call>\"}\n\n" +
		"event: done\ndata: {}\n\n"
	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(sse, `"name":"Bash"`) {
		t.Fatalf("思考里的工具调用没有被抽取：%q", sse)
	}
}

// TestTextToolCallDeduplicatedAcrossFragments 同一调用跨分片到达时只交付一次。
func TestTextToolCallDeduplicatedAcrossFragments(t *testing.T) {
	// 块被切成三段（模拟 SSE 分片）；每段之后都触发一次抽取。
	upstream := strings.Join([]string{
		"event: output\ndata: {\"response\":\"<opencode_tool_call>{\\\"name\\\":\\\"Read\\\",\"}\n\n",
		"event: output\ndata: {\"response\":\"\\\"input\\\":{\\\"file_path\\\":\\\"/tmp/a\\\"}}\"}\n\n",
		"event: output\ndata: {\"response\":\"</opencode_tool_call>\"}\n\n",
		"event: done\ndata: {}\n\n",
	}, "")
	sse, err := collect(t, upstream)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if count := strings.Count(sse, `"name":"Read"`); count != 1 {
		t.Fatalf("工具调用交付了 %d 次，应为 1 次：%q", count, sse)
	}
}

// TestUndeclaredToolCallFiltered 未声明的工具名不交付（防模型臆造）。
func TestUndeclaredToolCallFiltered(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"<opencode_tool_call>{\\\"name\\\":\\\"Invented\\\",\\\"input\\\":{}}</opencode_tool_call>\"}\n\n" +
		"event: done\ndata: {}\n\n"
	tr := newTranslator("glm-5.3")
	tr.allowedTools = map[string]bool{"read": true}
	r := newTranslateReader(io.NopCloser(strings.NewReader(upstream)), tr)
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	sse := string(data)
	if strings.Contains(sse, "Invented") {
		t.Fatalf("未声明的工具被交付给客户端（会让整轮失败）：%q", sse)
	}
	if tr.droppedTools != 1 {
		t.Errorf("应记录 1 个被丢弃的调用，实际 %d", tr.droppedTools)
	}
}

// TestDeclaredToolCallAllowed 声明过的工具正常交付（白名单不误伤）。
func TestDeclaredToolCallAllowed(t *testing.T) {
	upstream := "event: output\ndata: {\"response\":\"<opencode_tool_call>{\\\"name\\\":\\\"Read\\\",\\\"input\\\":{\\\"file_path\\\":\\\"/a\\\"}}</opencode_tool_call>\"}\n\n" +
		"event: done\ndata: {}\n\n"
	tr := newTranslator("glm-5.3")
	tr.allowedTools = map[string]bool{"read": true}
	r := newTranslateReader(io.NopCloser(strings.NewReader(upstream)), tr)
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(string(data), `"name":"Read"`) {
		t.Fatalf("声明过的工具被误拦：%q", string(data))
	}
}

// TestImageRequestDoesNotFallBackToRemote 带图请求在 3004 时也不回落 Remote。
//
// 回落会把图片悄悄丢掉（Remote 读不到内联图），用户拿到「没看图」的回答却无从
// 察觉——比如实报 3004 更糟。所以回落路径同样要过 requestHasImages 这道闸。
func TestImageRequestDoesNotFallBackToRemote(t *testing.T) {
	// 这条规则由 chatStreamLegacy 里的条件保证；这里断言判据本身在带图请求上为真，
	// 与 ChatStream 的入口判据共用同一函数（两处必须一致）。
	withImage, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/png;base64,QUJD"}}}}},
	})
	if !requestHasImages(withImage) {
		t.Fatal("带图请求必须被识别，否则会回落 Remote 并丢图")
	}
}

// TestImageRequestRoutesToLegacy 带图请求必须走 llm_utils_chat，不能走 Remote。
//
// 2026-10-16 三组对照实验的结论：Remote 端点读不到内联图片（媒体放 query、
// 放 initial_message.content、或两者都放，上游一律回「没有收到图片」，连目录里
// multimodal:true 的 kimi-k2.6 / Doubao-Seed-2.1-Pro 也一样）；同一个二进制、
// 同一张图，llm_utils_chat 路径答得完全正确（"有圆形和长方形，背景是蓝色"）。
func TestImageRequestRoutesToLegacy(t *testing.T) {
	withImage, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "describe"},
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/png;base64,QUJD",
			}},
		}}},
	})
	textOnly, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if !requestHasImages(withImage) {
		t.Error("带 image_url 的请求应判为有图片")
	}
	if requestHasImages(textOnly) {
		t.Error("纯文本请求不该判为有图片")
	}
	// Anthropic 形态（type=image + source）同样要认出来。
	anthropicImage, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "QUJD",
			}},
		}}},
	})
	if !requestHasImages(anthropicImage) {
		t.Error("Anthropic image 块应判为有图片")
	}
	// 解析失败保守判为「有图片」：宁可走 legacy，也不能静默丢图。
	if !requestHasImages([]byte("{not json")) {
		t.Error("解析失败应按有图片处理（保守）")
	}
	if requestHasImages(nil) {
		t.Error("空请求体不该判为有图片")
	}
}

// TestImagePartReachesRemoteQuery 图片部件在 Remote query 里的形态（保留：这条
// 路径现在只用于非图片请求的历史，但媒体块构造本身仍需正确）。
func TestImagePartReachesRemoteQuery(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "图里写了什么？"},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "aGVsbG8=",
			}},
		}},
	}
	query := flattenQueryWithImages(messages)
	var parts []any
	if err := json.Unmarshal([]byte(query), &parts); err != nil {
		t.Fatalf("query 不是合法 JSON 数组：%v（%q）", err, query)
	}
	var sawImage bool
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part["type"] != "image_url" {
			continue
		}
		data, _ := part["data"].(map[string]any)
		if data["data"] == "aGVsbG8=" && data["mimeType"] == "image/png" {
			sawImage = true
		}
	}
	if !sawImage {
		t.Fatalf("图片没有进入 query 的 image_url 部件：%s", query)
	}
	if strings.Contains(query, "[image omitted]") {
		t.Errorf("仍在使用占位文案而不是真图：%s", query)
	}
}

// TestBuildRemoteContentCarriesTextAndMedia content 数组必须同时带文本与图片。
//
// 对齐参考 build_web_content：文本部件与图片部件按消息顺序交替。
func TestBuildRemoteContentCarriesTextAndMedia(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "describe"},
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/png;base64,QUJD",
			}},
		}},
	}
	items := buildRemoteContent(messages)
	var sawText, sawImage bool
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "text":
			data, _ := item["data"].(map[string]any)
			if data["content"] == "describe" {
				sawText = true
			}
		case "image_url":
			value, _ := item["value"].(map[string]any)
			if value["data"] == "QUJD" && value["mimeType"] == "image/png" {
				sawImage = true
			}
		}
	}
	if !sawText {
		t.Errorf("content 缺文本部件：%#v", items)
	}
	if !sawImage {
		t.Errorf("content 缺图片部件：%#v", items)
	}
}

// TestStripImagePartsKeepsOtherBlocks 去图片只动图片块，文本与其它字段保留。
func TestStripImagePartsKeepsOtherBlocks(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "keep me"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}},
		}},
	}
	stripped := stripImageParts(messages)
	if len(stripped) != 1 {
		t.Fatalf("消息数应不变：%#v", stripped)
	}
	content, _ := stripped[0]["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("图片块应被去掉：%#v", content)
	}
	if block, _ := content[0].(map[string]any); block["text"] != "keep me" {
		t.Errorf("文本块被误改：%#v", content[0])
	}
	// 原消息不能被就地修改（避免影响同一轮里的其它用途）。
	original, _ := messages[0]["content"].([]any)
	if len(original) != 2 {
		t.Errorf("原消息被就地改写：%#v", original)
	}
}

// TestOpenAIImageURLReachesRemoteQuery OpenAI 形态的 image_url 同样要透传。
func TestOpenAIImageURLReachesRemoteQuery(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/jpeg;base64,/9j/4AAQ",
			}},
		}},
	}
	query := flattenQueryWithImages(messages)
	if !strings.Contains(query, `"mimeType":"image/jpeg"`) || !strings.Contains(query, `"/9j/4AAQ"`) {
		t.Fatalf("OpenAI 形态的图片没有透传：%s", query)
	}
}

// TestNoImageKeepsPlainQuery 无图请求的 query 形态不变（零回归）。
func TestNoImageKeepsPlainQuery(t *testing.T) {
	messages := []map[string]any{{"role": "user", "content": "纯文本问题"}}
	if got, want := flattenQueryWithImages(messages), flattenQuery(messages); got != want {
		t.Fatalf("无图时 query 应完全不变：\n got=%s\nwant=%s", got, want)
	}
}
