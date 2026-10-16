package trae

import (
	"os"
	"testing"
)

// TestParseCatalogBatchShape 验证 batch_get_detail_param 的响应能解析出模型能力与档位真名。
//
// 夹具是 2026-10-16 抓包（Trae 客户端真实响应）裁剪版：solo_agent 下 4 个模型，
// 保留了 config_name / display_config.multimodal / display_config.max_mode /
// model_detail_list 这几组**判据字段**。
//
// 为什么必须有这个测试：网关旧实现打的是 get_detail_param + function=solo_work_lite，
// 那个组合对 trae 账号恒返回空列表，于是 multimodal / max_mode 永远取不到，
// max 档从未生效。这个测试锁住「新接口形状能被正确解析」。
func TestParseCatalogBatchShape(t *testing.T) {
	data, err := os.ReadFile("testdata/catalog_batch_20261016.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	models, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("应解析出 4 个模型，实际 %d 个", len(models))
	}
	byName := map[string]CatalogModel{}
	for _, m := range models {
		byName[m.ConfigName] = m
	}

	// glm-5.3：目录标 multimodal=false（这正是它在直连时读不到图的原因），
	// 但 max_mode=true，且两档真名与上限都在。
	glm := byName["glm-5.3"]
	if glm.ConfigName == "" {
		t.Fatalf("glm-5.3 未解析出来")
	}
	if glm.Multimodal {
		t.Errorf("glm-5.3 的 multimodal 应为 false（目录口径），实际 true")
	}
	if !glm.MaxMode {
		t.Errorf("glm-5.3 应支持 max 档")
	}
	if glm.DevModelName != "glm-5.3__dev" {
		t.Errorf("glm-5.3 的 dev 真名应为 glm-5.3__dev，实际 %q", glm.DevModelName)
	}
	if glm.MaxModelName != "glm-5.3__max" {
		t.Errorf("glm-5.3 的 max 真名应为 glm-5.3__max，实际 %q", glm.MaxModelName)
	}

	// glm-5.3-flash：multimodal=true（能读图），两档上限不同。
	flash := byName["glm-5.3-flash"]
	if !flash.Multimodal {
		t.Errorf("glm-5.3-flash 的 multimodal 应为 true，实际 false")
	}
	if flash.DevPromptMax != 168000 {
		t.Errorf("glm-5.3-flash dev 档上限应为 168000，实际 %d", flash.DevPromptMax)
	}
	if flash.MaxPromptMax != 936000 {
		t.Errorf("glm-5.3-flash max 档上限应为 936000，实际 %d", flash.MaxPromptMax)
	}

	// deepseek-v4.1-flash：multimodal=true——与网关实测「它能读图」一致。
	if ds := byName["deepseek-v4.1-flash"]; !ds.Multimodal {
		t.Errorf("deepseek-v4.1-flash 的 multimodal 应为 true（实测它能读图）")
	}
}

// TestParseCatalogFlattensAndMergesFunctions 同一模型出现在多个 function 下时能力取并集。
//
// 客户端在哪个窗口用哪个 function 是运行期的事，网关不该因为查到的第一张表没标
// multimodal 就丢掉能力。
func TestParseCatalogFlattensAndMergesFunctions(t *testing.T) {
	data := []byte(`{"function_configs":[
	  {"function":"solo_agent","config_info_list":[
	    {"config_name":"glm-5.3","usage":"chat_completion",
	     "display_config":{"multimodal":false,"max_mode":false}}]},
	  {"function":"solo_coder","config_info_list":[
	    {"config_name":"glm-5.3","usage":"chat_completion",
	     "display_config":{"multimodal":true,"max_mode":true},
	     "model_detail_list":[{"model_name":"glm-5.3__max","prompt_max_tokens":936000}]}]}
	]}`)
	models, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("同名模型应合并成 1 条，实际 %d 条", len(models))
	}
	got := models[0]
	if !got.Multimodal || !got.MaxMode {
		t.Errorf("跨 function 的能力应取并集：multimodal=%v max_mode=%v", got.Multimodal, got.MaxMode)
	}
	if got.MaxModelName != "glm-5.3__max" {
		t.Errorf("max 档真名应保留，实际 %q", got.MaxModelName)
	}
}

// TestParseCatalogSkipsNonChatModels 辅助配置与内部子代理不进目录。
func TestParseCatalogSkipsNonChatModels(t *testing.T) {
	data := []byte(`{"function_configs":[{"function":"solo_agent","config_info_list":[
	  {"config_name":"summary","usage":"summary","display_config":{}},
	  {"config_name":"browser_use_subagent","usage":"chat_completion","is_invisible_to_user":true},
	  {"config_name":"glm-5.3","usage":"chat_completion","is_invisible_to_user":false}
	]}]}`)
	models, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(models) != 1 || models[0].ConfigName != "glm-5.3" {
		t.Fatalf("只应保留 glm-5.3，实际 %+v", models)
	}
}

// TestCatalogTracksFunctionMembership 目录要记住模型属于哪些 function。
//
// 夹具是 2026-10-16 真实响应裁剪版：glm-5.3-flash 在两张表里都有，
// qwen3.8-flash 只在 solo_agent 里（实测 solo_agent 67 个 vs solo_agent_remote 45 个）。
//
// 为什么必须记：网关按请求是否带工具在 solo_agent_remote / solo_work_remote 之间路由。
// 若目标 function 的表里没有这个模型，上游回 `config item is empty for config opt`
// （ConfigName 用了干净名，因为那张表没给 model_detail_list，取不到 __dev 真名）。
func TestCatalogTracksFunctionMembership(t *testing.T) {
	data, err := os.ReadFile("testdata/catalog_functions_20261016.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	models, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	byName := map[string]CatalogModel{}
	for _, m := range models {
		byName[m.ConfigName] = m
	}

	// glm-5.3-flash 两张表都有。
	flash := byName["glm-5.3-flash"]
	if !flash.Functions["solo_agent"] || !flash.Functions["solo_agent_remote"] {
		t.Errorf("glm-5.3-flash 应同时属于 solo_agent 与 solo_agent_remote，实际 %v", flash.Functions)
	}
	// qwen3.8-flash 只在 solo_agent。
	qwen := byName["qwen3.8-flash"]
	if !qwen.Functions["solo_agent"] {
		t.Errorf("qwen3.8-flash 应属于 solo_agent，实际 %v", qwen.Functions)
	}
	if qwen.Functions["solo_agent_remote"] {
		t.Errorf("qwen3.8-flash 不该属于 solo_agent_remote（真实目录里没有它），实际 %v", qwen.Functions)
	}

	// 路由判据：不在目标 function 里就该退回 solo_agent。
	if agentTypeSupportsModel(models, "qwen3.8-flash", "solo_agent_remote") {
		t.Errorf("qwen3.8-flash 不在 solo_agent_remote，agentTypeSupportsModel 应返回 false")
	}
	if !agentTypeSupportsModel(models, "qwen3.8-flash", "solo_agent") {
		t.Errorf("qwen3.8-flash 在 solo_agent，agentTypeSupportsModel 应返回 true")
	}
	if !agentTypeSupportsModel(models, "glm-5.3-flash", "solo_agent_remote") {
		t.Errorf("glm-5.3-flash 在 solo_agent_remote，应返回 true")
	}
	// 目录里没有的模型不拦（宁可按原路由试，让上游报错）。
	if !agentTypeSupportsModel(models, "unknown-model", "solo_agent_remote") {
		t.Errorf("未知模型不应被拦下")
	}
}
