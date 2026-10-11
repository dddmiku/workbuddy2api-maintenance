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

// rawModelsMain 打印一个真实模型条目的完整 JSON，用来确认倍率等字段名。
func rawModelsMain(authPath, want string) {
	raw, _ := os.ReadFile(authPath)
	a, err := auth.Parse(raw)
	if err != nil {
		fail("parse auth: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"function": "solo_work_lite", "config_names": nil, "need_prompt": false,
		"current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil,
	})
	req, _ := http.NewRequest(http.MethodPost,
		trae.GatewayBase+"/api/ide/v1/get_detail_param", bytes.NewReader(body))
	for k, vs := range trae.BuildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fail("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var parsed map[string]any
	json.Unmarshal(data, &parsed)
	list, _ := parsed["config_info_list"].([]any)
	if list == nil {
		if nested, ok := parsed["data"].(map[string]any); ok {
			list, _ = nested["config_info_list"].([]any)
		}
	}
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		name, _ := m["config_name"].(string)
		if name == "" {
			name, _ = m["name"].(string)
		}
		if name != want {
			continue
		}
		pretty, _ := json.MarshalIndent(m, "", " ")
		fmt.Println(string(pretty))
		return
	}
	fmt.Printf("config %q not found\n", want)
}
