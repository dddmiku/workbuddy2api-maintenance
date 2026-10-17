package trae

import (
	"encoding/json"
	"testing"
)

// catalogFixture 是 2026-10-11 真实 get_detail_param 响应的裁剪版
// （保留各过滤分支的代表项与真实倍率结构）。
const catalogFixture = `{"config_info_list":[
 {"config_name":"glm-5.3","display_name":"GLM-5.3","usage":"chat_completion",
  "is_invisible_to_user":false,
  "display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.78}},\"discount\":{\"enable\":true,\"data\":{\"original_consumption_rate\":0.78,\"consumption_rate\":0.39,\"member_discount\":50}}}"},
 {"config_name":"deepseek-v4.1-flash","display_name":"DeepSeek-V4.1-Flash","usage":"chat_completion",
  "is_invisible_to_user":false,
  "display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.15}}}"},
 {"config_name":"DeepSeek-V4-Pro","display_name":"DeepSeek-V4-Pro","usage":"chat_completion",
  "is_invisible_to_user":false,"display_contact_config":"{}"},
 {"config_name":"browser_use_subagent","display_name":"","usage":"chat_completion",
  "is_invisible_to_user":true},
 {"config_name":"summary","display_name":"","usage":"summary","is_invisible_to_user":false},
 {"config_name":"custom_model_gpt-5","display_name":"","usage":"custom_model","custom_models":{},
  "is_invisible_to_user":false}
]}`

func TestParseCatalogFiltersAndRates(t *testing.T) {
	models, err := parseCatalog([]byte(catalogFixture))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	byName := map[string]CatalogModel{}
	for _, m := range models {
		byName[m.ConfigName] = m
	}
	// 过滤：summary（usage 不对）、custom_model 不该出现。
	//
	// invisible 的语义在 2026-10-18 修正过：`is_invisible_to_user` 表示 Trae
	// 界面不显示，不是「不可调用」。kimi-k2 在 chat_v3 里标 invisible 却能正常
	// 出词，整条丢弃会让能跑的表消失（只剩 inline_chat → 3003）。所以现在
	// **保留**该条目并标 Hidden，只由 ModelIDsFromCatalog 决定不对外公布。
	for _, gone := range []string{"summary", "custom_model_gpt-5"} {
		if _, ok := byName[gone]; ok {
			t.Errorf("%q should have been filtered out", gone)
		}
	}
	if m, ok := byName["browser_use_subagent"]; !ok {
		t.Error("browser_use_subagent 应保留在目录里（标 Hidden），整条丢弃会让它的表从目录消失")
	} else if !m.Hidden {
		t.Error("browser_use_subagent 应标 Hidden（界面不显示）")
	}
	// 用户实测发现的漏项：deepseek-v4.1-flash 必须在内。
	if _, ok := byName["deepseek-v4.1-flash"]; !ok {
		t.Fatal("deepseek-v4.1-flash missing — this is the model the user actually uses")
	}
	// 倍率：有折扣取折后价（0.39），没折扣取原价（0.15），没给倍率则不写。
	if m := byName["glm-5.3"]; !m.HasRate || m.Rate != 0.39 {
		t.Errorf("glm-5.3 rate=%v has=%v want 0.39", m.Rate, m.HasRate)
	}
	if m := byName["deepseek-v4.1-flash"]; !m.HasRate || m.Rate != 0.15 {
		t.Errorf("deepseek-v4.1-flash rate=%v has=%v want 0.15", m.Rate, m.HasRate)
	}
	if m := byName["DeepSeek-V4-Pro"]; m.HasRate {
		t.Errorf("DeepSeek-V4-Pro should have no rate, got %v", m.Rate)
	}
}

func TestParseConsumptionRateShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     any
		want   float64
		wantOK bool
	}{
		{"discount wins over base", `{"consumption_rate":{"enable":true,"data":{"rate":0.78}},` +
			`"discount":{"enable":true,"data":{"consumption_rate":0.39}}}`, 0.39, true},
		{"base only", `{"consumption_rate":{"enable":true,"data":{"rate":0.15}}}`, 0.15, true},
		{"empty object", `{}`, 0, false},
		{"not json", `not json`, 0, false},
		{"nil", nil, 0, false},
		{"not a string", map[string]any{}, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseConsumptionRate(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestResolveFromCatalog(t *testing.T) {
	models, _ := parseCatalog([]byte(catalogFixture))
	cases := []struct{ in, want string }{
		{"glm-5.3", "glm-5.3"},                         // 配置名精确
		{"GLM-5.3", "glm-5.3"},                         // 忽略大小写
		{"DeepSeek-V4.1-Flash", "deepseek-v4.1-flash"}, // **显示名** → 配置名（用户会这么写）
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash"},
	}
	for _, tc := range cases {
		if got := ResolveFromCatalog(models, tc.in); got != tc.want {
			t.Errorf("ResolveFromCatalog(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestModelIDsFromCatalogIncludesBothSpellings(t *testing.T) {
	models, _ := parseCatalog([]byte(catalogFixture))
	ids := ModelIDsFromCatalog(models)
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	// 配置名与显示名都要公布：客户端可能按任一种写法请求。
	for _, want := range []string{"glm-5.3", "GLM-5.3", "deepseek-v4.1-flash", "DeepSeek-V4.1-Flash"} {
		if !seen[want] {
			t.Errorf("catalog should publish %q", want)
		}
	}
}

func TestParseCatalogRejectsMissingList(t *testing.T) {
	if _, err := parseCatalog([]byte(`{"other":1}`)); err == nil {
		t.Fatal("a response without config_info_list must be an error, not an empty catalog")
	}
}

var _ = json.Marshal
