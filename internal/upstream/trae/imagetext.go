// imagetext.go — 带图请求的「先转写再回答」链路（对齐官方客户端行为）。
//
// ═══ 为什么需要这一步（2026-10-18 实测）═══
//
// trae:glm-5.3 **没有视觉能力**。带内联图片的请求发到 llm_utils_chat，上游回
//
//	code=3004 "We're sorry, your requests have exceeded the rate limit."
//
// 这个文案是假的——同一账号、同一模型、纯文本请求 200 正常出词（对照实验见下）。
// 3004 在这里表达的是「本模型不支持图片」，上游却给了一个把人引向「等一会/换号」
// 的错误方向。
//
// A/B 对照（同一张 384×384 结构化 PNG：蓝底 + 红圆 + 绿矩形）：
//
//	trae:glm-5.3            带图 → 502（3004）   纯文本 → 200
//	trae:glm-5.3-flash      带图 → 200 正确读图
//	trae:deepseek-v4.1-flash 带图 → 200 正确读图
//	trae:multimodal_image_kimi-k2.5 带图 → 200 正确读图
//
// ═══ 官方客户端怎么做的 ═══
//
// 客户端**不把图直接给 glm-5.3**。它先用 multimodal_image_kimi-k2.5 把图转成
// 文字描述，再把描述喂给主模型（目录字段 read_tool_image_to_text_config_name）。
// 所以用户在 Trae 客户端里用 GLM-5.3 Max 贴图能得到准确回答——那一步看图的是
// kimi-k2.5，不是 glm-5.3。
//
// 本文件把这一步搬到网关侧：主模型被 3004 拒且本轮带图时，逐个把图片转写成
// 文本，替换掉图片块后用**同一个主模型**重发一次。
//
// ═══ 为什么在 3004 之后做，而不是出站前按 multimodal 标记预判 ═══
//
// 目录的 multimodal 标记不可靠（v3.8.4 曾据此出站前拦截，被用户实测推翻：目录
// 接口对这些 trae 账号返回过空列表，标记本身也可能是过期缓存）。按标记预判会把
// 「其实能读图」的组合误拦。改成「先让上游自己判」——它接受就正常返回，它拒绝
// 才走转写。代价是多一次上游往返，但零误拦。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// imageToTextModel 是客户端用来把图片转成文字的模型（目录字段
// read_tool_image_to_text_config_name 的值）。
const imageToTextModel = "multimodal_image_kimi-k2.5"

// imageToTextPrompt 是转写用的指令。要求结构化、可复述，避免转写模型自己
// 回答问题或省略细节——主模型只能看到这段文字，丢掉的信息就永久丢了。
const imageToTextPrompt = "请把这张图片完整转写成文字描述，供一个看不到图片的模型使用。要求：" +
	"1) 客观描述画面内容、布局、颜色、图形、文字（原文照抄，不要翻译）；" +
	"2) 如果是截图，逐项列出可见的界面元素、按钮、数值、报错信息；" +
	"3) 不要回答问题、不要给建议，只做描述；" +
	"4) 用中文，直接输出描述正文。"

// imageToTextTimeout 是单张图转写的超时。转写是同步阻塞的（主请求在等），
// 给足但不过分——实测 384×384 PNG 约 3s。
const imageToTextTimeout = 120 * time.Second

// transcribeImages 把 prepared 请求体里所有图片块转写成文本，返回新请求体。
//
// 返回 (nil, nil) 表示「没有图片」或「全部转写失败」，调用方据此决定是否放弃。
// 部分失败时保留成功的那几张——比整轮失败好。
func transcribeImages(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return nil, fmt.Errorf("transcribe: parse request: %w", err)
	}
	messages, ok := src["messages"].([]any)
	if !ok {
		return nil, nil
	}

	replaced, failed := 0, 0
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		blocks := make([]any, 0, len(content))
		changed := false
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				blocks = append(blocks, item)
				continue
			}
			media := rendererMediaBlock(block)
			if media == nil {
				blocks = append(blocks, item)
				continue
			}
			text, err := transcribeOneImage(ctx, a, media, clientIP)
			if err != nil {
				failed++
				log.Printf("WARN: [trae] 图片转写失败（保留原图块）：%v", err)
				blocks = append(blocks, item)
				continue
			}
			replaced++
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": "[图片内容（由 " + imageToTextModel + " 转写）]\n" + text,
			})
			changed = true
		}
		if !changed {
			out = append(out, raw)
			continue
		}
		copied := make(map[string]any, len(message))
		for k, v := range message {
			copied[k] = v
		}
		copied["content"] = blocks
		out = append(out, copied)
	}
	if replaced == 0 {
		return nil, nil
	}
	log.Printf("INFO: [trae] 已转写 %d 张图片为文本（失败 %d），用同一模型重发", replaced, failed)
	src["messages"] = out
	encoded, err := json.Marshal(src)
	if err != nil {
		return nil, fmt.Errorf("transcribe: encode request: %w", err)
	}
	return encoded, nil
}

// transcribeOneImage 调转写模型把一张图变成文字。
func transcribeOneImage(ctx context.Context, a *auth.Auth, media map[string]any, clientIP string) (string, error) {
	value, _ := media["value"].(map[string]any)
	mime := firstNonEmpty(stringFieldOf(value, "mimeType"), defaultImageMIME)
	data := stringFieldOf(value, "data")
	if data == "" {
		return "", fmt.Errorf("图片块没有 data")
	}
	var url string
	if mime == "url" {
		// 远程 URL：转写模型能自己取（与主请求同一出站通道）。
		url = data
	} else {
		url = "data:" + mime + ";base64," + data
	}

	// 请求体必须经 BuildChatBodyWithCatalog 构造，不能手搓：
	// llm_utils_chat 要求 function / config_name / model 三者同属一张目录表。
	// 手搓的 body 没有 function，上游回
	//   code 2001 "[LLMUtilsChat.resolveByUsage] function is empty"
	// 且响应里没有任何正文——2026-10-18 首版就是这样，日志表现为「空正文」。
	request := map[string]any{
		"model": imageToTextModel,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": imageToTextPrompt},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
			},
		}},
		"max_tokens": 2048,
	}
	prepared, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	var catalog []CatalogModel
	if models, cerr := FetchCatalog(ctx, a); cerr == nil && len(models) > 0 {
		catalog = models
		if resolved := ResolveFromCatalog(models, imageToTextModel); resolved != "" {
			prepared = withModel(prepared, resolved)
		}
	}
	body, err := BuildChatBodyWithCatalog(prepared, newRequestID(), catalog)
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, imageToTextTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		GatewayBaseVar+ChatPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	for k, vs := range buildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}

	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("转写模型 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	text := extractUpstreamText(raw)
	if strings.TrimSpace(text) == "" {
		// 把原始响应的开头一并记下：正文解析不出来时，只有看到真实字节才知道
		// 上游这次用的是什么形状（2026-10-18 首版只按 OpenAI 解析，日志里只有
		// 「空正文：id:1」，白排查一轮）。
		return "", fmt.Errorf("转写模型返回空正文（原始响应 %d 字节）：%s",
			len(raw), truncate(strings.ReplaceAll(string(raw), "\n", "\\n"), 400))
	}
	return text, nil
}

// extractUpstreamText 从上游响应里取出正文。
//
// ═══ 为什么不能只认 OpenAI 形状 ═══
//
// 转写请求打的是 llm_utils_chat——它回的是 **Trae 原生事件流**（event: output +
// {"response": "..."}），不是 OpenAI 的 choices[].delta.content。2026-10-18 首版
// 只按 OpenAI 解析，生产日志里表现为「转写模型返回空正文：id:1」——响应明明是
// 200，正文却取不到，于是整条链路失效。
//
// 所以这里同时认两种形态：Trae 原生（response 字段，且可能是**累计快照**）与
// OpenAI（choices[].delta/message.content，增量）。累计快照要按前缀合并，不能
// 直接拼接——否则同一段文字会出现多遍。
func extractUpstreamText(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	// 一次性 JSON（上游偶尔对非流式请求回整包）。
	if strings.HasPrefix(trimmed, "{") {
		var one map[string]any
		if err := json.Unmarshal(raw, &one); err == nil {
			if text := textFromTraePayload(one); text != "" {
				return text
			}
			return textFromOpenAIPayload(one)
		}
		return ""
	}

	// SSE：逐帧解析，Trae 原生与 OpenAI 两种都认。
	accumulator := newSnapshotAccumulator()
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		if text := textFromTraePayload(frame); text != "" {
			accumulator.add(text)
			continue
		}
		if text := textFromOpenAIPayload(frame); text != "" {
			accumulator.add(text)
		}
	}
	return accumulator.value()
}

// textFromTraePayload 取 Trae 原生事件里的正文（response 字段）。
func textFromTraePayload(obj map[string]any) string {
	if response, ok := obj["response"].(string); ok {
		// response 可能混着思考标记，剥掉后只留正文。
		_, visible := splitEmbeddedReasoning(response)
		return visible
	}
	return ""
}

// textFromOpenAIPayload 取 OpenAI 形状里的正文（delta 或 message）。
func textFromOpenAIPayload(obj map[string]any) string {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return ""
	}
	var builder strings.Builder
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			container, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			if content, ok := container["content"].(string); ok {
				builder.WriteString(content)
			}
		}
	}
	return builder.String()
}

// snapshotAccumulator 按「前缀快照 or 增量」两种语义合并正文分片。
//
// Trae 的 output 事件有时发完整快照（response 是到目前为止的全部文本），有时只发
// 增量——与 translator.text 的 add 同一套语义。这里用最小实现：新片段是已累积
// 文本的前缀延长就替换，否则当作增量追加。
type snapshotAccumulator struct {
	text strings.Builder
}

func newSnapshotAccumulator() *snapshotAccumulator { return &snapshotAccumulator{} }

func (s *snapshotAccumulator) add(chunk string) {
	if chunk == "" {
		return
	}
	current := s.text.String()
	if current == "" {
		s.text.WriteString(chunk)
		return
	}
	if strings.HasPrefix(chunk, current) {
		// 累计快照：整体替换。
		s.text.Reset()
		s.text.WriteString(chunk)
		return
	}
	if strings.HasPrefix(current, chunk) {
		// 重复片段（快照的旧前缀）：忽略。
		return
	}
	// 与尾部重叠的增量：Trae 的 response 常在快照后紧跟一段与尾部重叠的文本
	// （实测 `蓝` → `蓝色` → `色背景` 应得 `蓝色背景`，不是 `蓝色色背景`）。
	// 从最长可能的重叠开始找，找到即拼接剩余部分。
	limit := len(current)
	if len(chunk) < limit {
		limit = len(chunk)
	}
	for overlap := limit; overlap > 0; overlap-- {
		if strings.HasSuffix(current, chunk[:overlap]) {
			s.text.WriteString(chunk[overlap:])
			return
		}
	}
	s.text.WriteString(chunk)
}

func (s *snapshotAccumulator) value() string { return s.text.String() }
