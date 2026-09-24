// ═══ 更新日志 ═══
// 2026-09-25：锁定上下文错误的精确码匹配，普通错误引用超限文案时不得改变分类。
package upstream

import (
	"errors"
	"fmt"
	"testing"
)

func TestContextTooLongErrorDetailUsesExplicitCode(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		context          bool
	}{
		{"openai", `{"error":{"code":"context_length_exceeded","message":"too long"}}`, "too long", true},
		{"vendor", `{"code": 11115, "msg":"too long"}`, "too long", true},
		{"string-vendor", `{"code":"11115","msg":"too long"}`, "too long", true},
		{"extended", `{"extError":{"code":"context_length_exceeded","message":"too long"}}`, "too long", true},
		{"no-message", `{"code":11115}`, "request exceeds model context window", true},
		{"escaped", `{"code":11115,"msg":"100 \\u003e 99"}`, `100 \u003e 99`, true},
		{"auth-quote", `{"error":{"code":"invalid_api_key","message":"context_length_exceeded"}}`, "", false},
		{"loop-quote", `{"code":"upstream_reasoning_loop","message":"prompt is too long"}`, "", false},
		{"fractional", `{"code":11115.5,"msg":"too long"}`, "", false},
		{"long-code", `{"code":111150,"msg":"too long"}`, "", false},
		{"null", `null`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, ok := ContextTooLongHTTPDetail(400, []byte(tc.body))
			if ok != tc.context || message != tc.want {
				t.Fatalf("got %q,%t want %q,%t", message, ok, tc.want, tc.context)
			}
		})
	}
	if _, ok := ContextTooLongHTTPDetail(200, []byte(`{"code":11115}`)); ok {
		t.Fatal("successful HTTP response was classified as a rejected retry")
	}
}

func TestContextTooLongStreamDetailKeepsErrorType(t *testing.T) {
	contextFailure := &StreamError{Code: "upstream_error", Upstream: map[string]any{"code": float64(11115), "msg": "too long"}}
	for _, failure := range []error{contextFailure, fmt.Errorf("wrapped: %w", contextFailure)} {
		if message, ok := ContextTooLongStreamDetail(failure); !ok || message != "too long" {
			t.Fatalf("lost typed context error: %q %t", message, ok)
		}
	}
	for _, failure := range []error{nil, errors.New("context_length_exceeded"), &StreamError{Code: "upstream_reasoning_loop", Message: "context_length_exceeded"}} {
		if _, ok := ContextTooLongStreamDetail(failure); ok {
			t.Fatalf("unrelated error changed classification: %v", failure)
		}
	}
}
