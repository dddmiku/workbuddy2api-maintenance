// imagetext_test.go — 「带图 3004 → 转写图片 → 同一模型重发」链路的回归。
//
// 锁定的实测结论（2026-10-18，Oracle dev，同一账号交替对照，同一张 384×384
// 结构化 PNG：蓝底 + 红圆 + 绿矩形）：
//
//	trae:glm-5.3             带图 → 502（code 3004）   纯文本 → 200
//	trae:glm-5.3-flash       带图 → 200 正确读图
//	trae:deepseek-v4.1-flash 带图 → 200 正确读图
//	trae:multimodal_image_kimi-k2.5 带图 → 200 正确读图
//	转写文字再喂 trae:glm-5.3      → 200 正确回答
//
// 结论：3004 在带图请求上的含义是「该模型没有视觉能力」，不是工具轮次超限。
// 出路是把图转成文字（官方客户端就是这么做的），不是回落 Remote。
package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstreamerr"
)

// TestExtractUpstreamTextSSE 转写响应是 SSE 时也要能取到正文。
func TestExtractUpstreamTextSSE(t *testing.T) {
	raw := `event: output
data: {"choices":[{"delta":{"content":"蓝色"}}]}

data: {"choices":[{"delta":{"content":"背景"}}]}

data: [DONE]
`
	if got := extractUpstreamText([]byte(raw)); got != "蓝色背景" {
		t.Errorf("SSE 正文拼接错误：%q", got)
	}
}

// TestExtractUpstreamTextJSON 一次性 JSON 形态同样要认。
func TestExtractUpstreamTextJSON(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"红色圆形"}}]}`
	if got := extractUpstreamText([]byte(raw)); got != "红色圆形" {
		t.Errorf("JSON 正文提取错误：%q", got)
	}
}

// TestTranscribeImagesReplacesBlocks 图片块被替换成文本块，其它块原样保留。
func TestTranscribeImagesReplacesBlocks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"蓝底红圆绿矩形\"}}]}\n\n"))
	}))
	defer upstream.Close()
	old := GatewayBaseVar
	GatewayBaseVar = upstream.URL
	t.Cleanup(func() { GatewayBaseVar = old })

	prepared, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "这张图里有什么？"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,QUJD"}},
			},
		}},
	})

	out, err := transcribeImages(context.Background(), testAuth(), prepared, "")
	if err != nil {
		t.Fatalf("转写失败：%v", err)
	}
	if out == nil {
		t.Fatal("应返回改写后的请求体")
	}

	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("改写后的请求体不是合法 JSON：%v", err)
	}
	messages := decoded["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("块数应保持 2，实际 %d", len(content))
	}
	// 第一块（原文本）不动。
	if kind := content[0].(map[string]any)["type"]; kind != "text" {
		t.Errorf("原文本块被改动：%v", kind)
	}
	// 第二块（原图片）应变成文本。
	second := content[1].(map[string]any)
	if second["type"] != "text" {
		t.Errorf("图片块应替换成 text，实际 %v", second["type"])
	}
	if text, _ := second["text"].(string); !strings.Contains(text, "蓝底红圆绿矩形") {
		t.Errorf("转写正文未写入：%q", text)
	}
	// 请求体里不该再有图片块。
	if strings.Contains(string(out), "image_url") {
		t.Error("改写后仍残留 image_url 块")
	}
}

// TestTranscribeImagesNoImages 没有图片时不改写（返回 nil，让调用方知道没做事）。
func TestTranscribeImagesNoImages(t *testing.T) {
	prepared, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{
			"role": "user", "content": "纯文本提问",
		}},
	})
	out, err := transcribeImages(context.Background(), testAuth(), prepared, "")
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if out != nil {
		t.Error("无图片时应返回 nil")
	}
}

// TestImageRequestOn3004TranscribesNotRemote 带图请求被 3004 拒时，走转写重发，
// **不**回落 Remote（Remote 读不到内联图片，回落等于静默丢图）。
func TestImageRequestOn3004TranscribesNotRemote(t *testing.T) {
	remote := &remoteStub{createBody: `{"id":"sess-1","message_id":"m-1"}`,
		eventsBody: "event: output\ndata: {\"content\":\"SHOULD-NOT-APPEAR\"}\n\n"}
	remoteSrv := remote.start(t)
	defer remoteSrv.Close()
	oldRemote := RemoteBaseVar
	RemoteBaseVar = remoteSrv.URL
	t.Cleanup(func() { RemoteBaseVar = oldRemote })

	var resentBody []byte
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// 转写请求与主请求打同一个端点。判据用**转写指令文本**而不是模型名——
		// 转写后的正文里本身就含有 "multimodal_image_kimi-k2.5" 字样，用模型名
		// 会把重发请求误判成转写请求。
		if strings.Contains(string(raw), "完整转写成文字描述") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"蓝底红圆绿矩形\"}}]}\n\n"))
			return
		}
		if strings.Contains(string(raw), "image_url") {
			// 主请求第一次：带图，按上游实测回 3004。
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"error\":{\"code\":3004,\"message\":\"exceeded the rate limit\"}}\n\n"))
			return
		}
		// 主请求第二次：已转写，正常出词。
		//
		// 注意格式：主请求走 TranslateStreamWithTools，吃的是 **Trae 原生事件**
		// （event: output + {"response":...}），不是 OpenAI SSE。转写请求才用
		// OpenAI 格式（上面那个分支）。
		resentBody = raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: output\ndata: {\"response\":\"蓝色背景\"}\n\n" +
			"event: done\ndata: {}\n\n"))
	}))
	defer legacy.Close()
	oldGateway := GatewayBaseVar
	GatewayBaseVar = legacy.URL
	t.Cleanup(func() { GatewayBaseVar = oldGateway })

	prepared, _ := json.Marshal(map[string]any{
		"model": "glm-5.3",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "图里有什么"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,QUJD"}},
			},
		}},
	})

	primary := newStubReadCloser(
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n",
		&upstreamerr.Error{Code: 3004, Message: "exceeded the rate limit"})
	r := newRemoteFallbackReader(context.Background(), testAuth(), prepared, "", primary, true)

	var got strings.Builder
	buf := make([]byte, 4096)
	for i := 0; i < 40; i++ {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if remote.createCalls != 0 {
		t.Error("带图请求不该回落 Remote（会静默丢图）")
	}
	if resentBody == nil {
		t.Fatal("应发生一次转写后的重发")
	}
	if strings.Contains(string(resentBody), "image_url") {
		t.Error("重发的请求体里仍有图片块")
	}
	if !strings.Contains(got.String(), "蓝色背景") {
		t.Errorf("应交付重发后的正文，实际：%q", got.String())
	}
}
