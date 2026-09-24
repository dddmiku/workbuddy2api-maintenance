// ═══ 更新日志 ═══
// 2026-09-25：按明确错误码识别流内及重试超限，防止普通鉴权/循环错误的文案触发上下文恢复。
// 2026-09-25：首次 HTTP 与重试统一分类，仅为没有明确错误码的旧 prompt is too long 文本保留回退。
package upstream

import (
	"encoding/json"
	"errors"
	"strings"
)

func isContextTooLongCode(value any) bool {
	switch code := value.(type) {
	case string:
		return code == "context_length_exceeded" || code == "11115"
	case float64:
		return code == 11115
	case json.Number:
		number, err := code.Int64()
		return err == nil && number == 11115
	case int:
		return code == 11115
	}
	return false
}

// ContextTooLongErrorDetail recognizes explicit protocol codes only. Error text
// may quote another error and must not turn authentication or loop failures into
// context failures. The upstream object itself remains unchanged.
func ContextTooLongErrorDetail(failure map[string]any) (string, bool) {
	extended, _ := failure["extError"].(map[string]any)
	if !isContextTooLongCode(failure["code"]) && !isContextTooLongCode(extended["code"]) {
		return "", false
	}
	for _, object := range []map[string]any{failure, extended} {
		for _, field := range []string{"message", "msg"} {
			if value, ok := object[field].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value), true
			}
		}
	}
	return "request exceeds model context window", true
}

// ContextTooLongStreamDetail keeps context errors distinct from transport,
// parsing and loop failures after an upstream HTTP 200 stream has started.
func ContextTooLongStreamDetail(err error) (string, bool) {
	var failure *StreamError
	if !errors.As(err, &failure) {
		return "", false
	}
	if message, ok := ContextTooLongErrorDetail(failure.Upstream); ok {
		return message, true
	}
	return ContextTooLongErrorDetail(map[string]any{"code": failure.Code, "message": failure.Message})
}

// ContextTooLongHTTPDetail classifies both initial HTTP failures and retries.
// Explicit codes take precedence; legacy prompt-too-long text is accepted only
// without an explicit code. Merely quoting context_length_exceeded is not enough.
func ContextTooLongHTTPDetail(status int, body []byte) (string, bool) {
	if status < 400 {
		return "", false
	}
	var failure map[string]any
	if json.Unmarshal(body, &failure) != nil {
		return legacyContextTooLongDetail(strings.TrimSpace(string(body)))
	}
	objects := []map[string]any{failure}
	if nested, ok := failure["error"].(map[string]any); ok {
		objects = append([]map[string]any{nested}, objects...)
	}
	for _, object := range objects {
		if detail, ok := ContextTooLongErrorDetail(object); ok {
			return detail, true
		}
	}
	// Do not let another typed failure's message override its actual code.
	for _, object := range objects {
		extended, _ := object["extError"].(map[string]any)
		for _, candidate := range []map[string]any{object, extended} {
			if code, exists := candidate["code"]; exists && code != nil && code != "" {
				return "", false
			}
		}
	}
	for _, object := range objects {
		extended, _ := object["extError"].(map[string]any)
		for _, candidate := range []map[string]any{object, extended} {
			for _, field := range []string{"message", "msg"} {
				if value, ok := candidate[field].(string); ok {
					if detail, ok := legacyContextTooLongDetail(strings.TrimSpace(value)); ok {
						return detail, true
					}
				}
			}
		}
	}
	return "", false
}

func legacyContextTooLongDetail(message string) (string, bool) {
	if strings.Contains(message, "prompt is too long") {
		return message, true
	}
	return "", false
}
