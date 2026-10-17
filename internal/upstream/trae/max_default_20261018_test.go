// ═══ 更新日志 ═══
// 2026-10-18：默认档从 `__dev`（168000≈200K）改为 `__max`（936000≈1M）。
//
// 用户实测：客户端把模型窗口声明成 1M，但网关只发 `__dev`，实际永远跑 200K 档。
// 目录（batch_get_detail_param）里同一模型分两档，且**只有部分表给了 `__max`**：
//
//	solo_work_lite   glm-5.3  max_mode=false  只有 glm-5.3__dev              168000
//	solo_agent       glm-5.3  max_mode=true   glm-5.3__dev + glm-5.3__max   168000/936000
//	solo_agent_remote glm-5.3 max_mode=true   glm-5.3__dev + glm-5.3__max   168000/936000
//
// 所以「默认 max」不只是换真名，还必须把请求发到**给了 `__max` 的那张表**，
// 否则 function 与真名不配对，上游回 `config item is empty for config opt`。
package trae

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// maxTierFixture 两档模型 + 只有普通档的模型 + 单档模型。
const maxTierFixture = `{"function_configs":[
 {"function":"solo_work_lite","config_info_list":[
   {"config_name":"glm-5.3","usage":"chat_completion",
    "model_detail_list":[{"model_name":"glm-5.3__dev","prompt_max_tokens":168000}]},
   {"config_name":"glm-5.2","usage":"chat_completion",
    "model_detail_list":[{"model_name":"glm-5.2__dev","prompt_max_tokens":168000}]}]},
 {"function":"solo_agent","config_info_list":[
   {"config_name":"glm-5.3","usage":"chat_completion","max_mode":true,
    "model_detail_list":[{"model_name":"glm-5.3__dev","prompt_max_tokens":168000},
                         {"model_name":"glm-5.3__max","prompt_max_tokens":936000}]},
   {"config_name":"glm-5.2","usage":"chat_completion","max_mode":false,
    "model_detail_list":[{"model_name":"glm-5.2__dev","prompt_max_tokens":168000}]}]},
 {"function":"solo_agent_remote","config_info_list":[
   {"config_name":"glm-5.3","usage":"chat_completion","max_mode":true,
    "model_detail_list":[{"model_name":"glm-5.3__dev","prompt_max_tokens":168000},
                         {"model_name":"glm-5.3__max","prompt_max_tokens":936000}]}]},
 {"function":"solo_work_remote","config_info_list":[
   {"config_name":"glm-5.3","usage":"chat_completion","max_mode":false,
    "model_detail_list":[{"model_name":"glm-5.3__dev","prompt_max_tokens":168000}]}]}
]}`

func parseMaxTierFixture(t *testing.T) []CatalogModel {
	t.Helper()
	models, err := parseCatalog([]byte(maxTierFixture))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	return models
}

// TestResolveRequestedTierDefaultsToMax 两档模型不带后缀时默认 max 档。
func TestResolveRequestedTierDefaultsToMax(t *testing.T) {
	models := parseMaxTierFixture(t)

	base, wantMax := ResolveRequestedTier(models, "glm-5.3")
	if base != "glm-5.3" || !wantMax {
		t.Errorf("glm-5.3 应默认 max 档，实际 name=%q wantMax=%v", base, wantMax)
	}

	// 显示名同样要命中（客户端可能按 display_name 请求）。
	if base, wantMax := ResolveRequestedTier(models, "GLM-5.3"); base != "glm-5.3" || !wantMax {
		t.Errorf("显示名应映射到 glm-5.3 且默认 max，实际 name=%q wantMax=%v", base, wantMax)
	}

	// 只有普通档的模型不得被强行套 max（目录没标 max_mode）。
	if base, wantMax := ResolveRequestedTier(models, "glm-5.2"); base != "glm-5.2" || wantMax {
		t.Errorf("glm-5.2 无 max 档，应为普通档，实际 name=%q wantMax=%v", base, wantMax)
	}
}

// TestResolveRequestedTierExplicitDev 显式写 `-dev` 时退回普通档。
func TestResolveRequestedTierExplicitDev(t *testing.T) {
	models := parseMaxTierFixture(t)
	for _, name := range []string{"glm-5.3-dev", "glm-5.3_dev", "glm-5.3__dev"} {
		base, wantMax := ResolveRequestedTier(models, name)
		if base != "glm-5.3" || wantMax {
			t.Errorf("%s 应解析为 glm-5.3 普通档，实际 name=%q wantMax=%v", name, base, wantMax)
		}
	}
	// `-max` 仍可显式要求 max 档。
	if base, wantMax := ResolveRequestedTier(models, "glm-5.3-max"); base != "glm-5.3" || !wantMax {
		t.Errorf("glm-5.3-max 应解析为 glm-5.3 max 档，实际 name=%q wantMax=%v", base, wantMax)
	}
}

// TestFunctionForModelTierPicksTableWithMax max 档必须发到给了 `__max` 真名的表。
//
// 这是「默认 max」能否真正生效的关键一环：默认表 solo_work_lite 只有 `__dev`，
// 留在那张表里就还是 200K。
func TestFunctionForModelTierPicksTableWithMax(t *testing.T) {
	models := parseMaxTierFixture(t)

	if got := FunctionForModelTier(models, "glm-5.3", "solo_work_lite", true); got != "solo_agent" {
		t.Errorf("max 档应换到有 __max 真名的表（solo_agent），实际 %q", got)
	}
	// 普通档保持默认表（solo_work_lite 有 __dev）。
	if got := FunctionForModelTier(models, "glm-5.3", "solo_work_lite", false); got != "solo_work_lite" {
		t.Errorf("普通档应留在 solo_work_lite，实际 %q", got)
	}
	// 目录里根本没有 max 档：退回普通口径，不阻断出站。
	if got := FunctionForModelTier(models, "glm-5.2", "solo_work_lite", true); got != "solo_work_lite" {
		t.Errorf("无 max 档的模型应退回普通口径，实际 %q", got)
	}
}

// TestBuildChatBodyDefaultsToMaxTier llm_utils_chat 出站体默认发 `__max`。
func TestBuildChatBodyDefaultsToMaxTier(t *testing.T) {
	models := parseMaxTierFixture(t)
	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, err := BuildChatBodyWithCatalog(prepared, "req-test", models)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	var parsed struct {
		Model     string `json:"model"`
		ConfigName string `json:"config_name"`
		Function  string `json:"function"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body 应是合法 JSON：%v", err)
	}
	if parsed.Model != "glm-5.3__max" {
		t.Errorf("默认应发 __max 真名，实际 %q", parsed.Model)
	}
	if parsed.Function != "solo_agent" {
		t.Errorf("max 档必须发到有 __max 真名的表，实际 function=%q", parsed.Function)
	}
	if parsed.ConfigName != "glm-5.3" {
		t.Errorf("config_name 应保持裸名，实际 %q", parsed.ConfigName)
	}

	// 显式 -dev 时发普通档真名，且留在默认表。
	preparedDev, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3-dev",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	body, err = BuildChatBodyWithCatalog(preparedDev, "req-test2", models)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	parsed = struct {
		Model     string `json:"model"`
		ConfigName string `json:"config_name"`
		Function  string `json:"function"`
	}{}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Model != "glm-5.3__dev" || parsed.Function != "solo_work_lite" {
		t.Errorf("-dev 应发 __dev 真名到 solo_work_lite，实际 model=%q function=%q",
			parsed.Model, parsed.Function)
	}
}

// TestRemoteBodyAppliesMaxByDefault Remote 路径默认带 max 六件套并走 Agent 档。
func TestRemoteBodyAppliesMaxByDefault(t *testing.T) {
	models := parseMaxTierFixture(t)

	prepared, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file", "parameters": map[string]any{"type": "object"},
			},
		}},
	})
	body, agentType, maxApplied, err := buildRemoteBodyWithCatalog(context.Background(),
		testAuth(), prepared, models)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if !maxApplied {
		t.Error("默认应套 max 六件套")
	}
	if agentType != remoteDefaultAgentType {
		t.Errorf("max 档必须走 %s（只有它有 __max），实际 %s", remoteDefaultAgentType, agentType)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body 应是合法 JSON：%v", err)
	}
	init, _ := parsed["initial_message"].(map[string]any)
	if init == nil {
		t.Fatal("缺 initial_message")
	}
	if init["model_name"] != "glm-5.3__max" {
		t.Errorf("model_name 应为 glm-5.3__max，实际 %v", init["model_name"])
	}
	if init["model_selection_strategy"] != "max" {
		t.Errorf("model_selection_strategy 应为 max，实际 %v", init["model_selection_strategy"])
	}
	cw, _ := init["context_window_size"].(float64)
	if cw < 900000 {
		t.Errorf("context_window_size 应接近 1M，实际 %v", init["context_window_size"])
	}
	// 显式要普通档时不套 max 六件套，且保留 Work 档（工具语义优先）。
	preparedDev, _ := json.Marshal(map[string]any{
		"model":    "glm-5.3-dev",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file", "parameters": map[string]any{"type": "object"},
			},
		}},
	})
	_, agentType, maxApplied, err = buildRemoteBodyWithCatalog(context.Background(),
		testAuth(), preparedDev, models)
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	if maxApplied {
		t.Error("-dev 不该套 max 六件套")
	}
	if agentType != remoteWorkAgentType {
		t.Errorf("-dev + 工具应留在 %s，实际 %s", remoteWorkAgentType, agentType)
	}
}

// TestModelProfileOnlyReportsTablesWithMax modelProfile 只认「标了 max_mode 且有 __max 真名」的表。
func TestModelProfileOnlyReportsTablesWithMax(t *testing.T) {
	models := parseMaxTierFixture(t)
	if _, ok := modelProfile(models, "glm-5.3", "solo_work_lite"); ok {
		t.Error("solo_work_lite 没有 __max 真名，不该被报告为 max 档可用")
	}
	profile, ok := modelProfile(models, "glm-5.3", "solo_agent_remote")
	if !ok || !strings.HasSuffix(profile.MaxModelName, "__max") {
		t.Errorf("solo_agent_remote 应给出 __max 真名，实际 %+v ok=%v", profile, ok)
	}
}
