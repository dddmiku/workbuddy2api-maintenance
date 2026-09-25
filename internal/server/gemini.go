// ═══ 更新日志 ═══
// 2026-09-25：Gemini generate content 入口复用 Chat 执行与原始用量，不另建调度或密钥逻辑。
// 2026-09-25：模型发现共享调用权限并按真实元数据分页，未知上下文窗口保持省略。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/upstream"
)

// geminiContent is called after the shared authentication and body decoder.
// Wrapping those middleware in newGeminiWriter also translates their errors.
func (h *Handler) geminiContent(w http.ResponseWriter, r *http.Request) {
	gw, ok := w.(*geminiWriter)
	if !ok {
		gw = newGeminiWriter(w)
	}
	defer gw.finish()
	path := r.PathValue("modelAction")
	if path == "" {
		path = strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	}
	i := strings.LastIndex(path, ":")
	if i <= 0 || !geminiSafeModel(path[:i]) {
		gw.failure(400, "invalid_request", "model action must be a single model name followed by :generateContent or :streamGenerateContent")
		return
	}
	model, action := path[:i], path[i+1:]
	if action != "generateContent" && action != "streamGenerateContent" {
		gw.failure(501, "not_supported", "only generateContent and streamGenerateContent are supported; Interactions and exact countTokens are unavailable")
		return
	}
	expectedAlt := "json"
	if action == "streamGenerateContent" {
		expectedAlt = "sse"
	}
	if _, err := geminiQuery(r, expectedAlt); err != nil {
		gw.failure(400, "invalid_request", err.Error())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	if err != nil {
		writeBodyReadError(gw, err)
		return
	}
	if int64(len(body)) > h.cfg.MaxBodyBytes {
		gw.failure(413, "request_body_too_large", "request body exceeds the configured size limit")
		return
	}
	stream := action == "streamGenerateContent"
	chat, thoughts, err := geminiToChat(body, model, stream)
	if err != nil {
		gw.failure(400, "invalid_request", err.Error())
		return
	}
	gw.model, gw.stream = model, stream
	gw.includeThoughts = thoughts
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(chat))
	sub.ContentLength = int64(len(chat))
	h.chatCompletions(gw, sub)
}

func geminiSafeModel(model string) bool {
	return model != "" && !strings.ContainsAny(model, "\\%?#") && strings.IndexFunc(model, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) < 0
}

func geminiQuery(r *http.Request, expectedAlt string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("query parameters must use valid URL encoding")
	}
	for _, field := range []string{"alt", "pageSize", "pageToken"} {
		if len(query[field]) > 1 {
			return nil, fmt.Errorf("query parameter %s must not be repeated", field)
		}
	}
	if alt := query.Get("alt"); alt != "" && alt != expectedAlt {
		return nil, fmt.Errorf("this endpoint supports only alt=%s", expectedAlt)
	}
	return query, nil
}

func (h *Handler) geminiModels(w http.ResponseWriter, r *http.Request) {
	gw, ok := w.(*geminiWriter)
	if !ok {
		gw = newGeminiWriter(w)
	}
	gw.Header().Set("Cache-Control", "private, no-store")
	gw.Header().Set("X-Gateway-Capabilities", "/v1/capabilities")
	query, err := geminiQuery(r, "json")
	if err != nil {
		gw.failure(400, "invalid_request", err.Error())
		return
	}
	id := r.PathValue("model")
	if id == "" && strings.HasPrefix(r.URL.Path, "/v1beta/models/") {
		id = strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	}
	list := []map[string]any{}
	for _, source := range h.visibleModels(r) {
		name := stringField(source, "id")
		if !geminiSafeModel(name) {
			continue
		}
		display := stringField(source, "name")
		if display == "" {
			display = name
		}
		model := map[string]any{"name": "models/" + name, "displayName": display, "supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"}}
		if description := stringField(source, "description"); description != "" {
			model["description"] = description
		}
		for _, pair := range [][2]string{{"context_length", "inputTokenLimit"}, {"max_output_tokens", "outputTokenLimit"}} {
			if count, ok := upstream.UsageCount(source[pair[0]]); ok && count > 0 {
				model[pair[1]] = count
			}
		}
		if id != "" && name == id {
			gw.nativeJSON(200, model)
			return
		}
		list = append(list, model)
	}
	if id != "" {
		gw.failure(404, "model_not_found", "model is unavailable or this API key does not have access to it")
		return
	}
	pageSize := 100
	if value := query.Get("pageSize"); value != "" {
		size, err := strconv.Atoi(value)
		if err != nil || size < 1 || size > 1000 {
			gw.failure(400, "invalid_request", "pageSize must be between 1 and 1000")
			return
		}
		pageSize = size
	}
	after := ""
	if token := query.Get("pageToken"); token != "" {
		if len(token) > 8192 {
			gw.failure(400, "invalid_request", "invalid pageToken")
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(token)
		after = string(raw)
		if err != nil || !strings.HasPrefix(after, "models/") || !geminiSafeModel(strings.TrimPrefix(after, "models/")) {
			gw.failure(400, "invalid_request", "invalid pageToken")
			return
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i]["name"].(string) < list[j]["name"].(string) })
	start := sort.Search(len(list), func(i int) bool { return list[i]["name"].(string) > after })
	end := min(start+pageSize, len(list))
	result := map[string]any{"models": list[start:end]}
	if end < len(list) {
		result["nextPageToken"] = base64.RawURLEncoding.EncodeToString([]byte(list[end-1]["name"].(string)))
	}
	gw.nativeJSON(200, result)
}

func geminiToChat(body []byte, model string, stream bool) ([]byte, bool, error) {
	var source map[string]any
	if err := jsonutil.Decode(body, &source); err != nil || source == nil {
		return nil, false, fmt.Errorf("request body must be a JSON object")
	}
	if err := geminiOnlyFields(source, "request", "contents", "systemInstruction", "tools", "generationConfig", "toolConfig"); err != nil {
		return nil, false, err
	}
	messages, err := geminiHistory(source["contents"], source["systemInstruction"])
	if err != nil {
		return nil, false, err
	}
	chat := map[string]any{"model": model, "stream": stream, "messages": messages, "stream_options": map[string]any{"include_usage": true}}
	if source["tools"] != nil {
		tools, err := geminiTools(source["tools"])
		if err != nil {
			return nil, false, err
		}
		chat["tools"] = tools
	}
	if err := geminiToolConfig(source["toolConfig"], chat); err != nil {
		return nil, false, err
	}
	thoughts, err := geminiGenerationConfig(source["generationConfig"], chat)
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(chat)
	return encoded, thoughts, err
}

func geminiOnlyFields(object map[string]any, path string, allowed ...string) error {
	for key := range object {
		found := false
		for _, field := range allowed {
			if key == field {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s.%s is not supported by this gateway", path, key)
		}
	}
	return nil
}
