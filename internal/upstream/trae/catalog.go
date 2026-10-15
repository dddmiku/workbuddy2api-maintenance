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

// CatalogPath 模型目录端点（与 llm_utils_chat 同一个网关）。
const CatalogPath = "/api/ide/v1/get_detail_param"

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

func fetchCatalogUpstream(ctx context.Context, a *auth.Auth) ([]CatalogModel, error) {
	body, _ := json.Marshal(map[string]any{
		"function":            "solo_work_lite",
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		GatewayBaseVar+CatalogPath, bytes.NewReader(body))
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

// parseCatalog 解析 get_detail_param 的响应。
func parseCatalog(data []byte) ([]CatalogModel, error) {
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("trae catalog parse: %w", err)
	}
	list, _ := parsed["config_info_list"].([]any)
	if list == nil {
		if nested, ok := parsed["data"].(map[string]any); ok {
			list, _ = nested["config_info_list"].([]any)
		}
	}
	if list == nil {
		return nil, fmt.Errorf("trae catalog: no config_info_list in response")
	}
	models := make([]CatalogModel, 0, len(list))
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if model, ok := catalogModelFrom(item); ok {
			models = append(models, model)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ConfigName < models[j].ConfigName })
	return models, nil
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
	return model, true
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
