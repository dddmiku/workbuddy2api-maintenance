// catalog.go — Trae 域的动态模型目录（含消费倍率）。
//
// 为什么必须动态拉：模型清单是**账号/时段相关**的，静态表一定滞后。2026-10-11 实测：
// 我原先照参考项目的别名表写了一张静态清单，里面根本没有客户端真实在用的
// `deepseek-v4.1-flash`（Trae 面板里显示为 DeepSeek-V4.1-Flash），用户一眼就发现漏了。
//
// 上游端点：POST {gateway}/api/ide/v1/get_detail_param
// 返回 config_info_list，每项含 config_name / display_name / usage / is_invisible_to_user，
// 以及一个 **JSON 字符串** display_contact_config，倍率就在里面：
//
//	"consumption_rate":{"enable":true,"data":{"rate":0.78}}
//	"discount":{"enable":true,"data":{"original_consumption_rate":0.78,
//	             "consumption_rate":0.39,"member_discount":50}}
//
// 过滤规则（对齐参考实现）：
//   - usage 非 chat_completion 的（summary 之类）不是可选对话模型；
//   - custom_model_* 是路由配置不是模型；
//   - is_invisible_to_user=true 的（browser_use_subagent / explore_sub_agent_v2 等
//     内部子代理）不对外公布。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// CatalogPath 模型目录端点（旧形状，仅排障探针使用；主路径见 BatchCatalogPath）。
const CatalogPath = "/api/ide/v1/get_detail_param"

// BatchCatalogPath 是客户端真正使用的批量目录端点（2026-10-16 抓包确证）。
const BatchCatalogPath = "/api/ide/v1/batch_get_detail_param"

// catalogTTL 目录缓存时长。模型清单变化很慢，而这是展示端点——每次都打上游会把
// 展示变成高频调用，失败时还会反复重试。
const catalogTTL = 30 * time.Minute

// CatalogModel 目录里的一个模型。
type CatalogModel struct {
	// ConfigName 出站用的精确名（config_name），大小写敏感。
	ConfigName string
	// DisplayName 上游给人类看的名字（display_name），可能为空。
	DisplayName string
	// Rate 消费倍率。来自 consumption_rate.rate；有折扣时取折后价
	// （discount.consumption_rate）。0 表示上游没给倍率（不是"免费"）。
	Rate float64
	// HasRate 报告倍率是否真的从上游读到了。缺失 ≠ 0。
	HasRate bool
	// Multimodal 是否多模态（custom_model 要透传的字段）。
	Multimodal bool
	// MaxMode 账号目录是否标注该模型支持 1M max 档。
	//
	// 判据对齐参考 _fetch_solo_model_configs：顶层 max_mode 或
	// display_config.max_mode 任一为真。max 六件套**只在它为真时**生效——
	// 绝不凭空造 1M 上限（模型不支持会被上游拒）。
	MaxMode bool
	// DevModelName / MaxModelName 是 model_detail_list 里的**出站真名**。
	//
	// 2026-10-16 抓包确证：上游要求 model_name 带档位后缀，普通档 `__dev`、
	// 1M 档 `__max`（双下划线，不是单横线）。同一个 config_name 的两档
	// prompt_max_tokens 不同（glm-5.3-flash：168000 vs 936000）。
	// 网关旧实现按 `-max` 后缀猜，等于两档都没走对。
	DevModelName string
	MaxModelName string
	// DevPromptMax / MaxPromptMax 是两档各自的上下文上限（来自 model_detail_list）。
	DevPromptMax int64
	MaxPromptMax int64
}

// catalogEntry 是带时间的缓存项。
type catalogEntry struct {
	models  []CatalogModel
	fetched time.Time
	err     error
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]catalogEntry{}
)

// FetchCatalog 拉取指定账号的模型目录（带缓存）。
//
// 失败时返回上一次成功的快照（若有）并把错误一并返回：展示端点宁可显示稍旧的
// 真实清单，也不要因为一次网络抖动就变成空列表——空列表会让用户以为模型没了。
func FetchCatalog(ctx context.Context, a *auth.Auth) ([]CatalogModel, error) {
	key := a.UID
	catalogMu.Lock()
	cached, ok := catalogCache[key]
	catalogMu.Unlock()
	if ok && time.Since(cached.fetched) < catalogTTL {
		return cached.models, cached.err
	}

	models, err := fetchCatalogUpstream(ctx, a)
	catalogMu.Lock()
	if err != nil && ok && len(cached.models) > 0 {
		// 保留旧快照，只把错误记下来（下次还会重试）。
		catalogCache[key] = catalogEntry{models: cached.models, fetched: cached.fetched, err: err}
		catalogMu.Unlock()
		return cached.models, err
	}
	catalogCache[key] = catalogEntry{models: models, fetched: time.Now(), err: err}
	catalogMu.Unlock()
	return models, err
}

// InvalidateCatalog 作废某账号的目录缓存（面板「刷新模型」用）。
func InvalidateCatalog(uid string) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if uid == "" {
		catalogCache = map[string]catalogEntry{}
		return
	}
	delete(catalogCache, uid)
}

// catalogFunctions 是 batch_get_detail_param 要问的 function 列表。
//
// 必须一次问全：上游按 function 逐个返回 config_info_list，只问一个（旧实现问
// solo_work_lite）会拿到一张不完整的表——实测同一个账号 solo_agent 有 67 个模型，
// 而 solo_work_lite 只有 46 个，且**公开可用的模型列表不同**。列表抄自客户端抓包
// （2026-10-16），顺序与客户端一致。
var catalogFunctions = []string{
	"ui_builder_v2", "solo_coder", "chat_v3", "solo_builder", "builder_v3", "builder",
	"chat", "inline_chat", "git_ai", "custom_agent_generation", "utils", "code_reviewer",
	"code_review_summary", "solo_agent", "solo_agent_remote", "solo_work_remote",
	"solo_agent_lite", "solo_work_lite", "solo_design_lite", "solo_design_remote",
	"multimodal", "system_diagnosis",
}

func fetchCatalogUpstream(ctx context.Context, a *auth.Auth) ([]CatalogModel, error) {
	// ═══ 接口形状（2026-10-16 抓包修正）═══
	//
	// 旧实现打的是 POST /api/ide/v1/get_detail_param，body
	// {"function":"solo_work_lite",...}。实测该组合对 trae 账号**恒返回空列表**
	// （`{"config_info_list":[]}`），于是 max_mode / multimodal 全部取不到，
	// 网关只能退回静态别名表——「max 档从未生效」的根因就在这里。
	//
	// 客户端真实用的是 **batch_get_detail_param**，body 是
	// {"functions":[...22 个...],"agent_type":"solo_agent","current_config_info":
	// {"config_name":...,"is_custom_model":false},"biz_context":{"window_type":"Agent"}}，
	// 响应是 {"function_configs":[{"function":"solo_agent","config_info_list":[...]}]}。
	// 实测同一个账号该接口返回 2.17 MB / 373 条模型条目（旧接口 0 条）。
	//
	// agent_type 必须给 "solo_agent"：它决定每个 function 返回哪张模型表。
	body, _ := json.Marshal(map[string]any{
		"functions":                 catalogFunctions,
		"agent_type":                "solo_agent",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
		"biz_context":               map[string]any{"window_type": "Agent"},
	})
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		GatewayBaseVar+BatchCatalogPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range buildHeaders(a) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae catalog: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("trae catalog read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trae catalog: HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return parseCatalog(data)
}

// ParseCatalog 导出给排障探针：用**生产同一套**过滤与倍率解析规则验证真实响应。
func ParseCatalog(data []byte) ([]CatalogModel, error) { return parseCatalog(data) }

// parseCatalog 解析 batch_get_detail_param 的响应。
//
// 响应形状（2026-10-16 抓包确证）：
//
//	{"function_configs":[
//	   {"function":"solo_agent","config_info_list":[{...},...]},
//	   {"function":"solo_coder","config_info_list":[...]},
//	   ...]}
//
// 同一模型会在多个 function 下重复出现（glm-5.3 在 solo_agent / solo_coder 下都有），
// 按 config_name 去重：**能力字段取并集**（只要任一 function 标了 multimodal/max_mode
// 就认），因为客户端在哪个窗口用它是运行期的事，网关不该因为查到的第一张表没标就
// 丢掉能力。
//
// 兼容旧的扁平形状（config_info_list 直接在顶层）：排障探针 ParseCatalog 还会喂它。
func parseCatalog(data []byte) ([]CatalogModel, error) {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("trae catalog parse: %w", err)
	}
	// 收集所有 function 下的条目（扁平形状就是单张表）。
	var entries []map[string]any
	if groups, ok := parsed["function_configs"].([]any); ok {
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				continue
			}
			list, _ := group["config_info_list"].([]any)
			for _, raw := range list {
				if item, ok := raw.(map[string]any); ok {
					entries = append(entries, item)
				}
			}
		}
	}
	if len(entries) == 0 {
		list, _ := parsed["config_info_list"].([]any)
		if list == nil {
			if nested, ok := parsed["data"].(map[string]any); ok {
				list, _ = nested["config_info_list"].([]any)
			}
		}
		for _, raw := range list {
			if item, ok := raw.(map[string]any); ok {
				entries = append(entries, item)
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("trae catalog: no config_info_list in response")
	}
	merged := make(map[string]CatalogModel, len(entries))
	for _, item := range entries {
		model, ok := catalogModelFrom(item)
		if !ok {
			continue
		}
		if previous, seen := merged[model.ConfigName]; seen {
			merged[model.ConfigName] = mergeCatalogModel(previous, model)
			continue
		}
		merged[model.ConfigName] = model
	}
	models := make([]CatalogModel, 0, len(merged))
	for _, model := range merged {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ConfigName < models[j].ConfigName })
	return models, nil
}

// mergeCatalogModel 合并同一模型在不同 function 下的两条记录（能力取并集）。
func mergeCatalogModel(a, b CatalogModel) CatalogModel {
	out := a
	if b.DisplayName != "" {
		out.DisplayName = b.DisplayName
	}
	if !out.HasRate && b.HasRate {
		out.Rate = b.Rate
		out.HasRate = true
	}
	out.Multimodal = a.Multimodal || b.Multimodal
	out.MaxMode = a.MaxMode || b.MaxMode
	if out.DevModelName == "" {
		out.DevModelName = b.DevModelName
	}
	if out.MaxModelName == "" {
		out.MaxModelName = b.MaxModelName
	}
	if out.DevPromptMax == 0 {
		out.DevPromptMax = b.DevPromptMax
	}
	if out.MaxPromptMax == 0 {
		out.MaxPromptMax = b.MaxPromptMax
	}
	return out
}

// catalogModelFrom 把一个原始条目转成 CatalogModel；不可选时返回 false。
func catalogModelFrom(item map[string]any) (CatalogModel, bool) {
	// 只保留对话模型：summary 之类的辅助配置不是可选项。
	if usage, _ := item["usage"].(string); strings.TrimSpace(usage) != "chat_completion" {
		return CatalogModel{}, false
	}
	// custom_model_* 是路由配置，不是模型。
	if _, custom := item["custom_models"]; custom {
		return CatalogModel{}, false
	}
	// 内部子代理（browser_use_subagent / explore_sub_agent_v2 等）不对外公布。
	if invisible, ok := item["is_invisible_to_user"].(bool); ok && invisible {
		return CatalogModel{}, false
	}
	name, _ := item["config_name"].(string)
	if name == "" {
		name, _ = item["name"].(string)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return CatalogModel{}, false
	}
	display, _ := item["display_name"].(string)
	if display == "" {
		if dc, ok := item["display_config"].(map[string]any); ok {
			display, _ = dc["display_name"].(string)
		}
	}
	model := CatalogModel{ConfigName: name, DisplayName: strings.TrimSpace(display)}
	if rate, ok := parseConsumptionRate(item["display_contact_config"]); ok {
		model.Rate = rate
		model.HasRate = true
	}
	model.Multimodal = truthyFlag(item["multimodal"])
	model.MaxMode = truthyFlag(item["max_mode"])
	if dc, ok := item["display_config"].(map[string]any); ok {
		if truthyFlag(dc["max_mode"]) {
			model.MaxMode = true
		}
		if truthyFlag(dc["multimodal"]) {
			model.Multimodal = true
		}
	}
	// model_detail_list 给出该模型各档位的**出站真名**与上下文上限。
	// 档位后缀 `__dev` / `__max` 是上游硬要求（2026-10-16 抓包），
	// 这里把两档记下来，出站时按请求档位选用。
	if details, ok := item["model_detail_list"].([]any); ok {
		for _, raw := range details {
			detail, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := detail["model_name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			promptMax := int64(asFloatOrZero(detail["prompt_max_tokens"]))
			switch {
			case strings.HasSuffix(name, "__max"):
				if model.MaxModelName == "" {
					model.MaxModelName = name
					model.MaxPromptMax = promptMax
				}
			case strings.HasSuffix(name, "__dev"):
				if model.DevModelName == "" {
					model.DevModelName = name
					model.DevPromptMax = promptMax
				}
			}
		}
	}
	return model, true
}

// asFloatOrZero 把上游的数字字段（可能是 float64 / json.Number / 字符串）转成 float64。
func asFloatOrZero(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

// truthyFlag 上游布尔字段的三种拼写（bool / "true" / "1"）都认。
func truthyFlag(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true
		}
	}
	return false
}

// parseConsumptionRate 从 display_contact_config 里取消费倍率。
//
// 该字段是**JSON 字符串**（不是对象），内容形如：
//
//	{"consumption_rate":{"enable":true,"data":{"rate":0.78}},
//	 "discount":{"enable":true,"data":{"original_consumption_rate":0.78,
//	             "consumption_rate":0.39,"member_discount":50}}}
//
// 取值优先级：折后价（discount.consumption_rate）> 原价（consumption_rate.rate）。
// 折扣是「会员 5 折」这类当前生效的优惠，用户看到的、计费用的是折后价。
func parseConsumptionRate(value any) (float64, bool) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return 0, false
	}
	if discount, ok := cfg["discount"].(map[string]any); ok {
		if enabled, ok := discount["enable"].(bool); !ok || enabled {
			if data, ok := discount["data"].(map[string]any); ok {
				if rate, ok := asFloat(data["consumption_rate"]); ok && rate > 0 {
					return rate, true
				}
			}
		}
	}
	if base, ok := cfg["consumption_rate"].(map[string]any); ok {
		if data, ok := base["data"].(map[string]any); ok {
			if rate, ok := asFloat(data["rate"]); ok && rate > 0 {
				return rate, true
			}
		}
	}
	return 0, false
}

func asFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// ModelIDsFromCatalog 返回对外公布的模型名清单。
//
// 公布的是**配置名**（出站直接用的精确名）与显示名两者：客户端可能按任一种写法
// 请求，ResolveModelName 会把它们都映射到配置名。
func ModelIDsFromCatalog(models []CatalogModel) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(models)*2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, m := range models {
		add(m.ConfigName)
		add(m.DisplayName)
	}
	sort.Strings(out)
	return out
}

// ResolveFromCatalog 用实时目录把客户端模型名映射成配置名。
//
// 匹配顺序：配置名精确（含大小写）→ 配置名忽略大小写 → 显示名忽略大小写。
// 目录里没有时回落静态别名表（离线/首帧未拉到目录时仍可用）。
func ResolveFromCatalog(models []CatalogModel, name string) string {
	want := strings.TrimSpace(name)
	if want == "" {
		return ""
	}
	for _, m := range models {
		if m.ConfigName == want {
			return m.ConfigName
		}
	}
	lower := strings.ToLower(want)
	for _, m := range models {
		if strings.ToLower(m.ConfigName) == lower {
			return m.ConfigName
		}
	}
	for _, m := range models {
		if m.DisplayName != "" && strings.ToLower(m.DisplayName) == lower {
			return m.ConfigName
		}
	}
	return ResolveModelName(want)
}

// OutboundModelName 返回该模型在指定档位下的**出站真名**。
//
// 2026-10-16 抓包确证：上游要求 model_name 带档位后缀——普通档 `__dev`、
// 1M 档 `__max`（双下划线）。同一个 config_name 两档的上限不同
// （glm-5.3-flash：168000 vs 936000）。网关旧实现按单横线 `-max` 猜后缀，
// 两档都没走对。
//
// wantMax 为真且目录标了 max_mode 时给出 max 真名；否则给 dev 真名。
// 目录里没有 model_detail_list（或该档缺失）时**回落成 config_name 原样**——
// 上游对不带后缀的名字也能跑（实测 200），比凭空编一个后缀安全。
func (m CatalogModel) OutboundModelName(wantMax bool) string {
	if wantMax && m.MaxMode && m.MaxModelName != "" {
		return m.MaxModelName
	}
	if m.DevModelName != "" {
		return m.DevModelName
	}
	return m.ConfigName
}

// PromptMaxTokens 返回该档位的上下文上限（0 表示目录没给）。
func (m CatalogModel) PromptMaxTokens(wantMax bool) int64 {
	if wantMax && m.MaxMode && m.MaxPromptMax > 0 {
		return m.MaxPromptMax
	}
	return m.DevPromptMax
}

// LookupCatalogModel 按 config_name / display_name 在目录里找模型（大小写不敏感）。
func LookupCatalogModel(models []CatalogModel, name string) (CatalogModel, bool) {
	want := strings.TrimSpace(name)
	if want == "" {
		return CatalogModel{}, false
	}
	for _, m := range models {
		if m.ConfigName == want {
			return m, true
		}
	}
	lower := strings.ToLower(want)
	for _, m := range models {
		if strings.ToLower(m.ConfigName) == lower {
			return m, true
		}
	}
	for _, m := range models {
		if m.DisplayName != "" && strings.ToLower(m.DisplayName) == lower {
			return m, true
		}
	}
	return CatalogModel{}, false
}

// SplitMaxSuffix 拆出「请求名里的 max 意图」。
//
// 网关对外公布的档位写法是 `<model>-max`（trae:glm-5.3-max），与上游的
// `__max` 是两套写法：前者是**客户端意图**，后者是**出站真名**。这里只做前者。
func SplitMaxSuffix(name string) (base string, wantMax bool) {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	for _, suffix := range []string{"-max", "_max", "__max"} {
		if strings.HasSuffix(lower, suffix) {
			return trimmed[:len(trimmed)-len(suffix)], true
		}
	}
	return trimmed, false
}
