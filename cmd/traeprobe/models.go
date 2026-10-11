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

	body, _ := json.Marshal(map[string]any{
		"function":            "solo_work_lite",
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})

	req, err := http.NewRequest(http.MethodPost,
		trae.GatewayBase+"/api/ide/v1/get_detail_param", bytes.NewReader(body))
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
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
		fmt.Printf("  %-28s rate=%-7s display=%s\n", m.ConfigName, rate, m.DisplayName)
	}
}
