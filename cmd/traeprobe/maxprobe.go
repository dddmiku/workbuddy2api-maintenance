// maxprobe — 验证「默认 1M 档」在两条出站路径上是否真的发对了。
//
// 存在的理由：`__max` 的证据来自 Solo 会话抓包（batchInsert 的 model_info），
// 而 llm_utils_chat 的抓包 body 是加密的，没有直接证据证明它接受 `__max`。
// 只靠单元测试锁不住这一点——必须真发一次。
//
// 用法：traeprobe <auth文件> maxprobe <模型名> [--post]
//
// 打印：
//  1) 目录解析结果（config_name / 档位 / 选中的 function）
//  2) llm_utils_chat 出站体的关键字段（model / config_name / function）
//  3) Remote 建会话体的关键字段（model_name / agent_type / 六件套）
//  4) 带 --post 时真发 llm_utils_chat，打印 HTTP 状态与响应前若干字节
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

func maxprobeMain(authPath, model string, post bool) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	models, cerr := trae.FetchCatalog(ctx, a)
	if cerr != nil {
		fmt.Printf("目录拉取失败（%v），以下结论仅基于静态别名表\n", cerr)
	}
	base, wantMax := trae.ResolveRequestedTier(models, model)
	if base == "" {
		base = model
	}
	function := trae.FunctionForModelTier(models, base, "solo_work_lite", wantMax)
	fmt.Printf("目录: %d 个模型\n", len(models))
	fmt.Printf("解析: 请求=%q → config_name=%q wantMax=%v function=%q\n", model, base, wantMax, function)
	if cm, ok := trae.LookupCatalogModel(models, base); ok {
		fmt.Printf("目录标记: max_mode=%v dev=%q max=%q devMax=%d maxMax=%d\n",
			cm.MaxMode, cm.DevModelName, cm.MaxModelName, cm.DevPromptMax, cm.MaxPromptMax)
	}

	prepared, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "Say exactly: PONG"}},
		"stream":     true,
		"max_tokens": 64,
	})
	chatBody, err := trae.BuildChatBodyWithCatalog(prepared, "maxprobe-"+fmt.Sprint(time.Now().Unix()), models)
	if err != nil {
		fail("build chat body: %v", err)
	}
	printKeys("llm_utils_chat 出站体", chatBody, []string{"model", "config_name", "function"})

	remoteBody, agentType, maxApplied, err := trae.BuildRemoteBodyForProbe(ctx, a, prepared, models)
	if err != nil {
		fmt.Printf("Remote 建会话体构造失败：%v\n", err)
	} else {
		var parsed map[string]any
		_ = json.Unmarshal(remoteBody, &parsed)
		init, _ := parsed["initial_message"].(map[string]any)
		fmt.Printf("Remote: agent_type=%s maxApplied=%v model_name=%v strategy=%v context_window=%v prompt_max=%v\n",
			agentType, maxApplied, init["model_name"], init["model_selection_strategy"],
			init["context_window_size"], init["prompt_max_tokens"])
	}

	if !post {
		fmt.Println("（未发送；加 --post 真发 llm_utils_chat）")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		trae.GatewayBaseVar+trae.ChatPath, bytes.NewReader(chatBody))
	if err != nil {
		fail("build request: %v", err)
	}
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fail("post: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Printf("\n=== llm_utils_chat HTTP %d ===\n%s\n", resp.StatusCode, truncate(string(data), 2000))
}

// printKeys 只打印出站体的关键字段，避免把整段 messages 刷屏。
func printKeys(label string, body []byte, keys []string) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		fmt.Printf("%s: 解析失败 %v\n", label, err)
		return
	}
	fmt.Printf("%s:", label)
	for _, k := range keys {
		fmt.Printf(" %s=%v", k, parsed[k])
	}
	fmt.Println()
}
