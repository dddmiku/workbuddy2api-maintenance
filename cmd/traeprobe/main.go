// traeprobe 是 Trae 域的一次性端到端探针：真实上游 → 真实转换器 → 真实网关解析器。
//
// 存在的理由：密钥的模型绑定可能不含 trae 模型，走 HTTP 会先被绑定校验拦下，
// 那样测到的只是鉴权层而不是 Trae 链路。这个探针直接读账号文件、调用生产用的
// trae.ChatStream 与 upstream.Stream/Aggregate，把整条链路的真实行为打出来。
//
// 用法：traeprobe <auth文件路径> <模型名> [prompt]
package main

import (
	"bufio"
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
	// credits：打印账号积分包明细与到期时间。
	//
	// 存在的理由：trae 的积分包自带到期时间（Credits.Packs[].EndTime），而
	// 「优先消耗快过期积分」依赖池里的 credits_expiring。签到路径用的是不带分桶的
	// SetCredits，所以要看真实数据只能直接问上游。
	if model == "credits" {
		creditsMain(authPath)
		return
	}
	if model == "rawcredits" {
		rawcreditsMain(authPath)
		return
	}
	prompt := "Say exactly: PONG"
	if len(os.Args) > 3 {
		prompt = os.Args[3]
	}
	// --prompt-file：从文件读 prompt。板子 /bin/sh 的 argv 上限装不下复现 Claude Code
	// 全量上下文（>300KB）的命令行，只能走文件（2026-10-14 复现 trae 502 时发现）。
	if strings.HasPrefix(prompt, "--prompt-file=") {
		data, err := os.ReadFile(strings.TrimPrefix(prompt, "--prompt-file="))
		if err != nil {
			fail("read prompt file: %v", err)
		}
		prompt = string(data)
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

	// --timeline：逐行读原始上游流并打墙钟时间戳。回答「上游是迟迟不吐首字节，
	// 还是吐了首字节后中途断流」——两者对应完全不同的修复方向（前者要等得起，
	// 后者是连接被掐）。此前 printFrames 在整段读完后才解析，「110 秒零字节」
	// 无法区分这两种形态（2026-10-14 trae 502 排查时发现）。
	if hasFlag("--timeline") {
		rawRC, status, respBody, err := trae.RawChatStream(context.Background(), a, body, "")
		if err != nil {
			fail("trae raw stream: %v", err)
		}
		if rawRC == nil {
			fmt.Printf("upstream rejected: HTTP %d\n%s\n", status, truncate(string(respBody), 800))
			os.Exit(1)
		}
		defer rawRC.Close()
		t0 := time.Now()
		fmt.Printf("t=%s headers arrived\n", time.Since(t0).Round(time.Millisecond))
		br := bufio.NewReaderSize(rawRC, 64*1024)
		for {
			line, rerr := br.ReadString('\n')
			if line != "" {
				fmt.Printf("t=%s %s\n", time.Since(t0).Round(time.Millisecond),
					truncate(strings.TrimRight(line, "\r\n"), 100))
			}
			if rerr != nil {
				fmt.Printf("t=%s read end: %v\n", time.Since(t0).Round(time.Millisecond), rerr)
				break
			}
		}
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

// creditsMain 打印账号积分包明细与到期时间，用于判断「快过期优先消耗」是否有数据。
func creditsMain(authPath string) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	fmt.Printf("account: uid=%s realm=%s\n", a.UID, a.Realm())

	credits, err := trae.FetchCredits(context.Background(), a)
	if err != nil {
		fail("fetch credits: %v", err)
	}
	fmt.Printf("Total=%.0f Consumed=%.0f Remaining=%.0f Packs=%d\n",
		credits.Total, credits.Consumed, credits.Remaining, len(credits.Packs))

	now := time.Now()
	var expiring float64
	for i, p := range credits.Packs {
		if i >= 15 {
			fmt.Printf("  … 还有 %d 个包未列出\n", len(credits.Packs)-i)
			break
		}
		desc := "无到期时间"
		if p.EndTime > 0 {
			end := time.Unix(p.EndTime, 0)
			desc = fmt.Sprintf("%s（还有 %.1f 天）", end.Format("2006-01-02 15:04"), end.Sub(now).Hours()/24)
		}
		fmt.Printf("  pack %2d: name=%-22s limit=%-9.0f remaining=%-9.0f end=%s\n",
			i, truncate(p.Name, 22), p.Limit, p.Remaining, desc)
		// 与 upstream.expiringWithin 同口径：7 天窗口内的剩余额度。
		if p.EndTime > 0 && p.Remaining > 0 {
			if time.Unix(p.EndTime, 0).Before(now.Add(7 * 24 * time.Hour)) {
				expiring += p.Remaining
			}
		}
	}
	fmt.Printf("7 天内到期（expiring_soon 口径）: %.0f\n", expiring)
}

// rawcreditsMain 打印 ent_usage 的**原始** JSON，用于核对字段路径。
func rawcreditsMain(authPath string) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	data, err := trae.RawEntUsage(context.Background(), a)
	if err != nil {
		fail("raw ent usage: %v", err)
	}
	encoded, _ := json.MarshalIndent(data, "", " ")
	fmt.Println(string(encoded))
}
