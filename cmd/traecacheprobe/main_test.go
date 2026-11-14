package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosticFieldsNeverDecodeModelText(t *testing.T) {
	input := map[string]any{
		"content":                 `{"private_model_sentence":123}`,
		"reasoning_content":       `{"private_reasoning_sentence":456}`,
		"access_token":            "fixture-credential-do-not-output",
		"user_id":                 json.Number("1234567890123456"),
		"cache_read_input_tokens": json.Number("0"),
	}
	var got []field
	fields(input, "", &got, 0)
	encoded, _ := json.Marshal(got)
	for _, value := range []string{"private_model_sentence", "private_reasoning_sentence", "fixture-credential-do-not-output", "1234567890123456"} {
		if strings.Contains(string(encoded), value) {
			t.Error("diagnostics exposed a value or interpreted model text as field names")
		}
	}
	found := false
	for _, f := range got {
		if f.Path == ".cache_read_input_tokens" && f.Number == json.Number("0") {
			found = true
		}
	}
	if !found {
		t.Error("explicit numeric zero was lost")
	}
}
