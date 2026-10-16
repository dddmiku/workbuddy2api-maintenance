// traeprobe models — 拉取真实 Trae 模型目录（/api/ide/v1/get_detail_param）。
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

func modelsMain(authPath string) {
	raw, err := os.ReadFile(authPath)
	if err != nil {
		fail("read auth: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}

	// 与生产同一形状：batch_get_detail_param + 22 个 functions + agent_type。
	// 旧实现打的是 get_detail_param + function=solo_work_lite，那个组合对 trae
	// 账号恒返回空列表——探针必须走生产路径，否则排障看到的是假象。
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
		fail("build request: %v", err)
	}
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fail("request: %v", err)
	}
	defer resp.Body.Close()
	// 目录响应实测 2.17 MB（373 条模型条目），1 MiB 会把它截断成非法 JSON。
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	fmt.Printf("HTTP %d, %d bytes\n", resp.StatusCode, len(data))
	if resp.StatusCode != 200 {
		fmt.Println(truncate(string(data), 800))
		return
	}

	models, err := trae.ParseCatalog(data)
	if err != nil {
		fail("parse catalog: %v", err)
	}
	fmt.Printf("filtered catalog: %d models\n\n", len(models))
	for _, m := range models {
		rate := "-"
		if m.HasRate {
			rate = fmt.Sprintf("x%.2f", m.Rate)
		}
		// 能力与档位真名是这次修复的核心，探针必须一并打印，否则排障看不到。
		fmt.Printf("  %-28s rate=%-7s mm=%-5v max=%-5v dev=%-24s maxname=%-24s\n",
			m.ConfigName, rate, m.Multimodal, m.MaxMode, m.DevModelName, m.MaxModelName)
	}
}
