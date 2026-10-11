// traeprobe 是 Trae 域的一次性端到端探针：真实上游 → 真实转换器 → 真实网关解析器。
//
// 存在的理由：密钥的模型绑定可能不含 trae 模型，走 HTTP 会先被绑定校验拦下，
// 那样测到的只是鉴权层而不是 Trae 链路。这个探针直接读账号文件、调用生产用的
// trae.ChatStream 与 upstream.Stream/Aggregate，把整条链路的真实行为打出来。
//
// 用法：traeprobe <auth文件路径> <模型名> [prompt]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: traeprobe <auth-file> <model> [prompt]")
		os.Exit(2)
	}
	authPath, model := os.Args[1], os.Args[2]
	if model == "models" {
		modelsMain(authPath)
		return
	}
	if model == "rawmodel" {
		want := "glm-5.3"
		if len(os.Args) > 3 {
			want = os.Args[3]
		}
		rawModelsMain(authPath, want)
		return
	}
	prompt := "Say exactly: PONG"
	if len(os.Args) > 3 {
		prompt = os.Args[3]
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	fmt.Printf("account: uid=%s realm=%s domain=%s\n", a.UID, a.Realm(), a.Domain)
	if a.Realm() != trae.Realm {
		fail("account realm is %q, expected %q", a.Realm(), trae.Realm)
	}

	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"stream":     true,
		"max_tokens": 256,
	})
	// --tools：加一个简单工具并强制调用，覆盖工具链路（首片带 id/name、
	// 后续只带参数片段的跨帧合并）。
	if len(os.Args) > 4 && os.Args[4] == "--tools" {
		body, _ = json.Marshal(map[string]any{
			"model": model,
			"messages": []any{map[string]any{
				"role":    "user",
				"content": "Use the read_file tool to read the file /etc/hostname. Call the tool, do not answer in text.",
			}},
			"stream":     true,
			"max_tokens": 256,
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "read_file",
					"description": "Read a file from disk and return its contents.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string", "description": "Absolute file path"},
						},
						"required": []any{"path"},
					},
				},
			}},
			"tool_choice": "auto",
		})
	}

	// ── 1) 流式：真实上游 → 转换器 → upstream.Stream ──
	fmt.Println("\n=== stream ===")
	start := time.Now()

	// --upstream：先看**上游原始事件**（转换器之前）。工具调用的 name/arguments
	// 具体落在哪个事件、哪个字段，只有这里能看清。
	if hasFlag("--upstream") {
		rawRC, status, respBody, err := trae.RawChatStream(context.Background(), a, body, "")
		if err != nil {
			fail("trae raw stream: %v", err)
		}
		if rawRC == nil {
			fmt.Printf("upstream rejected: HTTP %d\n%s\n", status, truncate(string(respBody), 800))
			os.Exit(1)
		}
		defer rawRC.Close()
		data, _ := io.ReadAll(rawRC)
		fmt.Println(truncate(string(data), 6000))
		return
	}

	rc, status, respBody, err := trae.ChatStream(context.Background(), a, body, "")
	if err != nil {
		fail("trae.ChatStream transport: %v", err)
	}
	if rc == nil {
		fmt.Printf("upstream rejected: HTTP %d\n%s\n", status, truncate(string(respBody), 600))
		os.Exit(1)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	streamErr := upstream.Stream(rec, rc)
	elapsed := time.Since(start)
	out := rec.Body.String()
	fmt.Printf("gateway Stream err=%v  elapsed=%s\n", streamErr, elapsed.Round(time.Millisecond))
	printFrames(out)

	// ── 2) 聚合：真实上游 → 转换器 → upstream.Aggregate ──
	fmt.Println("\n=== aggregate (non-stream view) ===")
	rc2, status2, respBody2, err := trae.ChatStream(context.Background(), a, body, "")
	if err != nil {
		fail("trae.ChatStream transport: %v", err)
	}
	if rc2 == nil {
		fmt.Printf("upstream rejected: HTTP %d\n%s\n", status2, truncate(string(respBody2), 600))
		os.Exit(1)
	}
	defer rc2.Close()
	chat, aggErr := upstream.Aggregate(rc2)
	if aggErr != nil {
		fmt.Printf("gateway Aggregate err=%v\n", aggErr)
		os.Exit(1)
	}
	summary, _ := json.MarshalIndent(chat, "", " ")
	fmt.Println(truncate(string(summary), 2000))
}

// printFrames 只打印转换器产出的帧摘要（逐帧 type + 关键字段），避免刷屏。
func printFrames(sse string) {
	n := 0
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		n++
		if payload == "[DONE]" {
			fmt.Printf("  frame %d: [DONE]\n", n)
			continue
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			fmt.Printf("  frame %d: (unparsable) %s\n", n, truncate(payload, 120))
			continue
		}
		var parts []string
		if choices, ok := frame["choices"].([]any); ok {
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				for _, key := range []string{"role", "content", "reasoning_content"} {
					if v, ok := delta[key].(string); ok && v != "" {
						parts = append(parts, fmt.Sprintf("%s=%q", key, truncate(v, 60)))
					}
				}
				if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
					b, _ := json.Marshal(calls)
					parts = append(parts, "tool_calls="+truncate(string(b), 300))
				}
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					parts = append(parts, "finish="+fr)
				}
			}
		}
		if u, ok := frame["usage"].(map[string]any); ok && len(u) > 0 {
			parts = append(parts, "usage="+compact(u))
		}
		if len(parts) == 0 {
			parts = append(parts, "(empty delta)")
		}
		fmt.Printf("  frame %d: %s\n", n, strings.Join(parts, " "))
	}
	fmt.Printf("  total frames: %d\n", n)
}

func compact(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// hasFlag 报告命令行里是否出现某个开关（位置不限）。
func hasFlag(name string) bool {
	for _, arg := range os.Args {
		if arg == name {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

var _ = bytes.MinRead
