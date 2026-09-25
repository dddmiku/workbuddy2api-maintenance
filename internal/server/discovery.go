// ═══ 更新日志 ═══
// 2026-09-25：公开Gemini生成能力及有限上下文控制，默认图片工具声明的过滤同样可见。
// 2026-09-25：模型详情与发现共用密钥权限，支持 Anthropic 的 API key 请求头，并如实公开网关能力与兼容降级。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
)

// Discovery shares calling-key authentication, including expiration and model
// allowlists. Authorization has the same precedence as /v1/messages; an invalid
// Bearer header must never be rescued by a different X-API-Key value.
func (h *Handler) withDiscoveryAuth(next http.HandlerFunc) http.HandlerFunc {
	authenticated := h.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-API-Key") != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+r.Header.Get("X-API-Key"))
		} else if r.Header.Get("Authorization") == "" && r.Header.Get("X-Goog-Api-Key") != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+r.Header.Get("X-Goog-Api-Key"))
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Gateway-Capabilities", "/v1/capabilities")
		authenticated(w, r)
	}
}

func (h *Handler) visibleModels(r *http.Request) []map[string]any {
	list := h.modelList()
	info, restricted := requestKeyInfo(r)
	if !restricted || len(info.Models) == 0 {
		return list
	}
	filtered := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		id, _ := entry["id"].(string)
		if id != "" && modelAllowedByKey(info, id) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (h *Handler) model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model")
	for _, entry := range h.visibleModels(r) {
		if entry["id"] == id {
			_ = writeJSON(w, http.StatusOK, entry)
			return
		}
	}
	// Hidden and nonexistent models are indistinguishable to a restricted key.
	writeOpenAIError(w, http.StatusNotFound, "model_not_found", "model is unavailable or this API key does not have access to it")
}

func (h *Handler) capabilities(w http.ResponseWriter, _ *http.Request) {
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"object":             "gateway.capabilities",
		"protocols":          []string{"chat_completions", "responses", "anthropic_messages", "gemini_generate_content"},
		"streaming":          true,
		"models_path":        "/v1/models",
		"model_capabilities": "use metadata reported for each model; omitted values are unknown",
		"tools": map[string]any{
			"function":                     "model_dependent",
			"custom":                       "responses_function_bridge",
			"namespace":                    "responses_function_bridge",
			"server_executed":              []string{},
			"ignored_default_declarations": []string{"web_search", "tool_search", "image_generation"},
			"ignored_declaration_policy":   "accepted_with_warning",
			"forced_unsupported_choice":    "rejected",
			"warning_header":               "X-WB2API-Ignored-Tools",
			"delivery":                     "after_complete_batch_validation",
		},
		"gemini": map[string]any{
			"models_path": "/v1beta/models", "generate_content": true, "stream_generate_content": true,
			"interactions": false, "cached_content": false, "opaque_thought_signatures": false,
			"unknown_thought_usage": "aggregate_output_with_explicit_gatewayUsage_metadata",
		},
		"anthropic_context_management": "keep_all_thinking_only",
		"native_compaction":            false,
		"response_storage":             false,
		"exact_token_counting":         false,
		"codex_model_manifest":         false,
		"usage":                        map[string]any{"source": "upstream", "input_multiplier": 1},
		"request_body_encodings":       []string{"identity", "gzip", "zstd"},
		"request_id_header":            "X-Request-ID",
	})
}

func warnIgnoredBuiltinToolsJSON(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var tools []any
	if json.Unmarshal(raw, &tools) == nil {
		warnIgnoredBuiltinTools(w, r, tools)
	}
}

func warnIgnoredBuiltinTools(w http.ResponseWriter, r *http.Request, tools []any) {
	ignored := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		kind, _ := tool["type"].(string)
		if !isUnimplementedBuiltinTool(kind) {
			continue
		}
		if kind == "image_generation" {
			ignored["image_generation"] = true
			continue
		}
		// Report only fixed family names, never unbounded user-controlled header
		// text from dated/future variants or their extra fields.
		for _, family := range builtinToolPrefixes {
			if kind == family || strings.HasPrefix(kind, family+"_") || strings.HasPrefix(kind, family+"-") {
				ignored[family] = true
			}
		}
	}
	if len(ignored) == 0 {
		return
	}
	names := make([]string, 0, len(ignored))
	for name := range ignored {
		names = append(names, name)
	}
	sort.Strings(names)
	value := strings.Join(names, ", ")
	w.Header().Set("X-WB2API-Ignored-Tools", value)
	w.Header().Set("X-Gateway-Capabilities", "/v1/capabilities")
	w.Header().Add("Warning", `299 workbuddy2api "Unsupported built-in tool declarations were ignored: `+value+`"`)
	log.Printf("WARN: [server] rid=%s ignored_builtin_tools=%s; see /v1/capabilities", requestID(r), strings.Join(names, ","))
}
