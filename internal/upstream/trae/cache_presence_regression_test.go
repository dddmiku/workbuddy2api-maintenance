package trae

import (
	"strings"
	"testing"
)

func TestUsageCachePresenceAndCanonicalDetails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    map[string]any
		cached any
	}{
		{"missing", map[string]any{"input_token": float64(10), "output_token": float64(2)}, nil},
		{"explicit_zero", map[string]any{"input_token": float64(10), "output_token": float64(2), "cache_read_input_tokens": float64(0)}, int64(0)},
		{"flat_hit", map[string]any{"cached_tokens": float64(8)}, int64(8)},
		{"nested_hit", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(8)}}, int64(8)},
		{"nested_zero", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(0)}}, int64(0)},
		{"fraction_is_unknown", map[string]any{"cached_tokens": float64(0.25)}, nil},
		{"negative_is_unknown", map[string]any{"cached_tokens": float64(-1)}, nil},
		{"null_is_unknown", map[string]any{"cached_tokens": nil}, nil},
		{"invalid_nested_uses_flat", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": "invalid"}, "cache_read_input_tokens": float64(8)}, int64(8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := mapUsage(tc.raw)
			details, _ := out["prompt_tokens_details"].(map[string]any)
			if details["cached_tokens"] != tc.cached {
				t.Fatalf("cached field presence/value lost: got=%v want=%v", details["cached_tokens"], tc.cached)
			}
		})
	}
	if out := mapUsage(nil); out != nil {
		t.Error("absent usage fabricated a total_tokens observation")
	}
	if out := mapUsage(map[string]any{"input_token": float64(10)}); out["total_tokens"] != nil {
		t.Error("partial usage fabricated a complete total")
	}
}

func TestRemoteUsagePreservesCachePresence(t *testing.T) {
	for _, tc := range []struct{ name, fields, want string }{
		{"missing", "", ""},
		{"explicit_zero", `,"cache_read_input_tokens":0`, `"cached_tokens":0`},
		{"nested_hit", `,"prompt_tokens_details":{"cached_tokens":8}`, `"cached_tokens":8`},
		{"reasoning", `,"reasoning_tokens":2`, `"reasoning_tokens":2`},
		{"creation", `,"cache_creation_input_tokens":4`, `"cache_creation_input_tokens":4`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			converted, ok := (&remoteEventConverter{}).convert("token_usage", `{"prompt_tokens":10,"completion_tokens":2`+tc.fields+`}`)
			if !ok {
				t.Fatal("Remote discarded valid usage")
			}
			translated, err := collect(t, converted+"event: done\ndata: {}\n\n")
			if err != nil {
				t.Fatal("Remote usage failed downstream translation")
			}
			if tc.want != "" && !strings.Contains(translated, tc.want) {
				t.Fatal("Remote lost the cache observation before translation")
			}
			if tc.want == "" && strings.Contains(translated, "cached_tokens") {
				t.Fatal("Remote invented a cache observation")
			}
		})
	}
}
