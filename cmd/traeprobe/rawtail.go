// rawtail — 打印 llm_utils_chat 原始事件的**尾部**（含 token_usage / done），
// 用于确认上游是否真的下发 credit 字段。--upstream 只打前 6000 字节，看不到尾部。
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

func rawtailMain(authPath, model string) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	models, _ := trae.FetchCatalog(ctx, a)
	prepared, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Say exactly: PONG"}},
		"stream":     true,
		"max_tokens": 64,
	})
	body, err := trae.BuildChatBodyWithCatalog(prepared, "rawtail-"+fmt.Sprint(time.Now().Unix()), models)
	if err != nil {
		fail("build: %v", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, trae.GatewayBaseVar+trae.ChatPath, bytes.NewReader(body))
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		fail("post: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	fmt.Printf("HTTP %d, %d bytes\n", resp.StatusCode, len(data))
	// 只打最后 4000 字节：token_usage 与 done 在尾部。
	tail := data
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	fmt.Printf("=== tail ===\n%s\n", string(tail))
}
