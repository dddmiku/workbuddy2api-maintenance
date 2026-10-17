// creditprobe — 探测 get_session_usage 能否给出真实扣费（credits_consumed）。
//
// 背景：llm_utils_chat 的 token_usage 事件只带 token 数，**没有 credit 字段**
// （2026-10-18 原始事件确认）。官方客户端另打
// POST {gateway}/api/v1/commercial/get_session_usage {"session_id": "<turn id>"}
// 拿 credits_consumed。这个探针验证：
//  1) llm_utils_chat 的 metadata 事件里的 session_id 能否当 turn id 用；
//  2) 我们发出去的 request_id 能否当 turn id 用；
//  3) Remote 建会话拿到的 message_id 能否当 turn id 用；
//  4) 结算是否有延迟（连续轮询看值何时出现）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

const sessionUsagePath = "/api/v1/commercial/get_session_usage"

// sessionUsage 查询一次会话扣费，返回原始响应体。
func sessionUsage(ctx context.Context, a *auth.Auth, turnID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"session_id": turnID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trae.GatewayBaseVar+sessionUsagePath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-lgw-req-sdk-type", "3")
	req.Header.Set("package-type", "stable_cn")
	req.Header.Set("app-version", "default")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(string(data), 700)), nil
}

func creditprobeMain(authPath, model string) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	models, _ := trae.FetchCatalog(ctx, a)
	requestID := fmt.Sprintf("creditprobe-%d", time.Now().UnixNano())
	prepared, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Say exactly: PONG"}},
		"stream":     true,
		"max_tokens": 64,
	})
	body, err := trae.BuildChatBodyWithCatalog(prepared, requestID, models)
	if err != nil {
		fail("build: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		trae.GatewayBaseVar+trae.ChatPath, bytes.NewReader(body))
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		fail("post: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("llm_utils_chat HTTP %d, %d bytes, request_id=%s\n", resp.StatusCode, len(data), requestID)

	// 从事件流里抠出所有可能的 turn id 候选。
	candidates := map[string]string{"request_id(我们发的)": requestID}
	text := string(data)
	for _, line := range bytes.Split([]byte(text), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &obj) != nil {
			continue
		}
		for _, key := range []string{"session_id", "prompt_completion_id", "message_id", "turn_id", "id"} {
			if v, ok := obj[key]; ok && v != nil {
				if s := fmt.Sprint(v); s != "" && s != "0" {
					candidates["事件字段 "+key] = s
				}
			}
		}
	}
	fmt.Println("候选 turn id：")
	for label, id := range candidates {
		fmt.Printf("  %s = %s\n", label, id)
	}

	fmt.Println("\n=== 逐个试探（间隔 2s，各试 3 次看结算延迟）===")
	for label, id := range candidates {
		for attempt := 1; attempt <= 3; attempt++ {
			out, err := sessionUsage(ctx, a, id)
			if err != nil {
				fmt.Printf("%s [%s] #%d 错误：%v\n", label, id, attempt, err)
			} else {
				fmt.Printf("%s [%s] #%d %s\n", label, id, attempt, out)
			}
			if attempt < 3 {
				time.Sleep(2 * time.Second)
			}
		}
	}
}
