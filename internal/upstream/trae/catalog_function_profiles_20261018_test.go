package trae

import (
	"encoding/json"
	"testing"
)

// 2026-10-18 实测数据（生产目录 live 抓取，1467216630920908 账号）。
//
// 这组用例锁住「同一 config_name 在不同 function 表里对应不同 model_name」，
// 以及「只在专用表里的模型必须发到专用表」。这两件事任一漏掉，上游都回
// `config item is empty for config opt`：
//
//	deepseek-V3.1        只在 code_reviewer，真名 deepseek-V3.1__dev
//	code-review-judge    只在 code_reviewer，真名却是 deepseek-V3.1__dev（映射到别的模型）
//	doubao-2.0-mini      只在 git_ai
//	multimodal_image_kimi-k2.5 只在 multimodal
//	kimi-k2              只在 chat_v3 / builder / inline_chat，真名 kimi-k2__dev
//	qwen-3.6-plus        在 solo_coder（__dev）与 chat（__v2）里真名不同
const functionProfileFixture = `{"function_configs":[
 {"function":"solo_work_lite","config_info_list":[
   {"config_name":"glm-5.3","usage":"chat_completion",
    "model_detail_list":[{"model_name":"glm-5.3__dev","prompt_max_tokens":168000},
                         {"model_name":"glm-5.3__max","prompt_max_tokens":936000}]},
   {"config_name":"Doubao-Seed-2.0-Code","usage":"chat_completion",
    "model_detail_list":[{"model_name":"Doubao-Seed-2.1-Turbo__dev","prompt_max_tokens":168000}]}]},
 {"function":"code_reviewer","config_info_list":[
   {"config_name":"deepseek-V3.1","usage":"chat_completion",
    "model_detail_list":[{"model_name":"deepseek-V3.1__dev","prompt_max_tokens":128000}]},
   {"config_name":"code-review-judge","usage":"chat_completion",
    "model_detail_list":[{"model_name":"deepseek-V3.1__dev","prompt_max_tokens":96000}]}]},
 {"function":"git_ai","config_info_list":[
   {"config_name":"doubao-2.0-mini","usage":"chat_completion",
    "model_detail_list":[{"model_name":"doubao-2.0-mini","prompt_max_tokens":30000}]}]},
 {"function":"multimodal","config_info_list":[
   {"config_name":"multimodal_image_kimi-k2.5","usage":"chat_completion",
    "model_detail_list":[{"model_name":"multimodal_image_kimi-k2.5","prompt_max_tokens":168000}]}]},
 {"function":"chat_v3","config_info_list":[
   {"config_name":"kimi-k2","usage":"chat_completion",
    "model_detail_list":[{"model_name":"kimi-k2__dev","prompt_max_tokens":100000}]}]},
 {"function":"solo_coder","config_info_list":[
   {"config_name":"qwen-3.6-plus","usage":"chat_completion",
    "model_detail_list":[{"model_name":"qwen-3.6-plus__dev","prompt_max_tokens":168000}]}]},
 {"function":"chat","config_info_list":[
   {"config_name":"qwen-3.6-plus","usage":"chat_completion",
    "model_detail_list":[{"model_name":"qwen-3.6-plus","prompt_max_tokens":30000},
                         {"model_name":"qwen-3.6-plus__v2","prompt_max_tokens":45000}]}]}
]}`

func parseFunctionProfileFixture(t *testing.T) []CatalogModel {
	t.Helper()
	models, err := parseCatalog([]byte(functionProfileFixture))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	return models
}

// TestCatalogKeepsPerFunctionModelNames 每个 function 各存一份出站真名。
func TestCatalogKeepsPerFunctionModelNames(t *testing.T) {
	models := parseFunctionProfileFixture(t)
	byName := map[string]CatalogModel{}
	for _, m := range models {
		byName[m.ConfigName] = m
	}

	// code-review-judge 在 code_reviewer 表里的真名是 deepseek-V3.1__dev。
	judge := byName["code-review-judge"]
	if got := judge.OutboundModelNameFor("code_reviewer", false); got != "deepseek-V3.1__dev" {
		t.Errorf("code-review-judge 在 code_reviewer 的真名应为 deepseek-V3.1__dev，实际 %q", got)
	}

	// qwen-3.6-plus 两张表真名不同：solo_coder 用 __dev，chat 用干净名/__v2。
	qwen := byName["qwen-3.6-plus"]
	if got := qwen.OutboundModelNameFor("solo_coder", false); got != "qwen-3.6-plus__dev" {
		t.Errorf("qwen-3.6-plus 在 solo_coder 的真名应为 qwen-3.6-plus__dev，实际 %q", got)
	}
	if got := qwen.OutboundModelNameFor("chat", false); got == "qwen-3.6-plus__dev" {
		t.Errorf("qwen-3.6-plus 在 chat 表里不该沿用 solo_coder 的 __dev 真名，实际 %q", got)
	}

	// 只有一张表时按表取。
	if got := byName["kimi-k2"].OutboundModelNameFor("chat_v3", false); got != "kimi-k2__dev" {
		t.Errorf("kimi-k2 在 chat_v3 的真名应为 kimi-k2__dev，实际 %q", got)
	}
	if got := byName["doubao-2.0-mini"].OutboundModelNameFor("git_ai", false); got != "doubao-2.0-mini" {
		t.Errorf("doubao-2.0-mini 在 git_ai 的真名应为 doubao-2.0-mini，实际 %q", got)
	}
}

// TestCatalogFunctionProfileUnknownFunctionFallsBack 未知 function 回落全局口径。
func TestCatalogFunctionProfileUnknownFunctionFallsBack(t *testing.T) {
	models := parseFunctionProfileFixture(t)
	var glm CatalogModel
	for _, m := range models {
		if m.ConfigName == "glm-5.3" {
			glm = m
		}
	}
	if got := glm.OutboundModelNameFor("no_such_function", false); got != "glm-5.3__dev" {
		t.Errorf("未知 function 应回落到全局 dev 真名，实际 %q", got)
	}
}

// TestFunctionForModelPicksTableThatHasIt 只在专用表里的模型要发到那张表。
func TestFunctionForModelPicksTableThatHasIt(t *testing.T) {
	models := parseFunctionProfileFixture(t)
	cases := []struct{ model, want string }{
		{"deepseek-V3.1", "code_reviewer"},
		{"code-review-judge", "code_reviewer"},
		{"doubao-2.0-mini", "git_ai"},
		{"multimodal_image_kimi-k2.5", "multimodal"},
		{"kimi-k2", "chat_v3"},
	}
	for _, tc := range cases {
		// preferred 是网关默认的 solo_work_lite —— 这些模型都不在里面。
		if got := FunctionForModel(models, tc.model, "solo_work_lite"); got != tc.want {
			t.Errorf("FunctionForModel(%q) = %q，应为 %q", tc.model, got, tc.want)
		}
	}
	// 默认表里有的模型保持默认，不要乱换。
	if got := FunctionForModel(models, "glm-5.3", "solo_work_lite"); got != "solo_work_lite" {
		t.Errorf("glm-5.3 应留在 solo_work_lite，实际 %q", got)
	}
	// 目录里没有的模型原样返回 preferred（让上游如实报错）。
	if got := FunctionForModel(models, "not-a-model", "solo_work_lite"); got != "solo_work_lite" {
		t.Errorf("未知模型应保持 preferred，实际 %q", got)
	}
}

// TestBuildChatBodyWithCatalogUsesPerFunctionName 出站体必须 function 与真名成对。
//
// 只换 function 不换真名（或反过来）上游都回 `config item is empty for config opt`，
// 所以这里两者一起断言。
func TestBuildChatBodyWithCatalogUsesPerFunctionName(t *testing.T) {
	models := parseFunctionProfileFixture(t)
	cases := []struct {
		model        string
		wantFunction string
		wantName     string
	}{
		{"deepseek-V3.1", "code_reviewer", "deepseek-V3.1__dev"},
		{"code-review-judge", "code_reviewer", "deepseek-V3.1__dev"},
		{"doubao-2.0-mini", "git_ai", "doubao-2.0-mini"},
		{"kimi-k2", "chat_v3", "kimi-k2__dev"},
		{"glm-5.3", "solo_work_lite", "glm-5.3__dev"},
	}
	for _, tc := range cases {
		prepared, _ := json.Marshal(map[string]any{
			"model":    tc.model,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		body, err := BuildChatBodyWithCatalog(prepared, "req-1", models)
		if err != nil {
			t.Fatalf("BuildChatBodyWithCatalog(%s): %v", tc.model, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if parsed["function"] != tc.wantFunction {
			t.Errorf("%s: function=%v want %v", tc.model, parsed["function"], tc.wantFunction)
		}
		if parsed["model"] != tc.wantName {
			t.Errorf("%s: model=%v want %v", tc.model, parsed["model"], tc.wantName)
		}
		if parsed["config_name"] != tc.model {
			t.Errorf("%s: config_name=%v want %v", tc.model, parsed["config_name"], tc.model)
		}
	}
}

// TestBuildChatBodyWithoutCatalogKeepsLegacyShape 目录不可用时保持旧行为。
//
// 缺目录时改道比原样发出去更糟：上游的报错比网关猜的路由更准确。
func TestBuildChatBodyWithoutCatalogKeepsLegacyShape(t *testing.T) {
	prepared := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	body, err := BuildChatBodyWithCatalog(prepared, "req-1", nil)
	if err != nil {
		t.Fatalf("BuildChatBodyWithCatalog: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if parsed["function"] != "solo_work_lite" {
		t.Errorf("无目录时 function 应保持 solo_work_lite，实际 %v", parsed["function"])
	}
	if parsed["model"] != "glm-5.3" {
		t.Errorf("无目录时 model 应保持请求名，实际 %v", parsed["model"])
	}
}

// TestRemoteAgentTypeRejectsNonRemoteTables Remote 服务不了的模型要返回空串。
//
// 2026-10-18 生产实测：Remote 建会话即使成功，收流仍按 solo_agent 表判
// `config item is empty`。所以只在 code_reviewer / git_ai / multimodal /
// chat 等非 Remote 表里的模型必须返回空串 —— 调用方据此直接走 legacy。
func TestRemoteAgentTypeRejectsNonRemoteTables(t *testing.T) {
	models := parseFunctionProfileFixture(t)
	for _, name := range []string{"deepseek-V3.1", "code-review-judge", "doubao-2.0-mini",
		"multimodal_image_kimi-k2.5", "kimi-k2"} {
		if got := remoteAgentTypeForModel(models, name, "solo_work_remote"); got != "" {
			t.Errorf("%s 只在非 Remote 表里，应返回空串（改走 legacy），实际 %q", name, got)
		}
	}
	// 目录里没有的模型保持 preferred（不因缺数据改道）。
	if got := remoteAgentTypeForModel(models, "not-a-model", "solo_work_remote"); got != "solo_work_remote" {
		t.Errorf("未知模型应保持 preferred，实际 %q", got)
	}
}
