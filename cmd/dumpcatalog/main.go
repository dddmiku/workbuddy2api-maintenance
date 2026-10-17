// dumpcatalog 把 Trae 目录的**原始响应**写到文件，供字段对比用。
//
// 存在理由：网关公布的 40 个模型里，客户端下拉只显示 18 个。差异必然来自目录
// 里某个字段，但网关当前只解析了 is_invisible_to_user / multimodal / max_mode。
// 要找到真正的判据，必须看原始条目——不能靠猜字段名。
//
// 用法：dumpcatalog <auth文件> <输出文件>
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: dumpcatalog <auth-file> <out.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "read auth:", err)
		os.Exit(1)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse auth:", err)
		os.Exit(1)
	}

	body, _ := json.Marshal(map[string]any{
		"functions": []string{
			"ui_builder_v2", "solo_coder", "chat_v3", "solo_builder", "builder_v3", "builder",
			"chat", "inline_chat", "git_ai", "custom_agent_generation", "utils", "code_reviewer",
			"code_review_summary", "solo_agent", "solo_agent_remote", "solo_work_remote",
			"solo_agent_lite", "solo_work_lite", "solo_design_lite", "solo_design_remote",
			"multimodal", "system_diagnosis",
		},
		"agent_type":                "solo_agent",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
		"biz_context":               map[string]any{"window_type": "Agent"},
	})
	req, err := http.NewRequest(http.MethodPost,
		trae.GatewayBase+trae.BatchCatalogPath, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	fmt.Printf("HTTP %d, %d bytes\n", resp.StatusCode, len(data))
	if err := os.WriteFile(os.Args[2], data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", os.Args[2])
}
