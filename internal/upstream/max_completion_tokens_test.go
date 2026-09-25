// ═══ 更新日志 ═══
// 2026-09-25：锁定输出预算别名的实际出站字段、显式预算优先级与数字保真，不推测零值或无效参数的语义。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestPrepareBodyMaxCompletionTokensAlias(t *testing.T) {
	cases := []struct {
		name      string
		fields    string
		wantLimit string
		wantAlias string
	}{
		{"missing", "", "", ""},
		{"existing_limit", `,"max_tokens":17`, "17", ""},
		{"one_token_alias", `,"max_completion_tokens":1`, "1", ""},
		{"positive_alias", `,"max_completion_tokens":4096`, "4096", ""},
		{"null_limit", `,"max_tokens":null,"max_completion_tokens":4096`, "4096", ""},
		{"smaller_explicit_limit_wins", `,"max_tokens":17,"max_completion_tokens":4096`, "17", ""},
		{"larger_explicit_limit_wins", `,"max_tokens":4096,"max_completion_tokens":17`, "4096", ""},
		{"alias_above_float_precision", `,"max_completion_tokens":9007199254740993`, "9007199254740993", ""},
		{"alias_int64_boundary", `,"max_completion_tokens":9223372036854775807`, "9223372036854775807", ""},
		{"explicit_limit_above_float_precision", `,"max_tokens":9007199254740993,"max_completion_tokens":17`, "9007199254740993", ""},
		{"null_alias_unchanged", `,"max_completion_tokens":null`, "", "null"},
		{"zero_alias_unchanged", `,"max_completion_tokens":0`, "", "0"},
		{"negative_zero_alias_unchanged", `,"max_completion_tokens":-0`, "", "-0"},
		{"negative_alias_unchanged", `,"max_completion_tokens":-1`, "", "-1"},
		{"decimal_alias_unchanged", `,"max_completion_tokens":1.5`, "", "1.5"},
		{"exponent_alias_unchanged", `,"max_completion_tokens":1e3`, "", "1e3"},
		{"string_alias_unchanged", `,"max_completion_tokens":"4096"`, "", `"4096"`},
		{"boolean_alias_unchanged", `,"max_completion_tokens":true`, "", "true"},
		{"overflow_alias_unchanged", `,"max_completion_tokens":9223372036854775808`, "", "9223372036854775808"},
		{"zero_explicit_limit_unchanged", `,"max_tokens":0,"max_completion_tokens":4096`, "0", "4096"},
		{"negative_explicit_limit_unchanged", `,"max_tokens":-1,"max_completion_tokens":4096`, "-1", "4096"},
		{"decimal_explicit_limit_unchanged", `,"max_tokens":1.5,"max_completion_tokens":4096`, "1.5", "4096"},
		{"string_explicit_limit_unchanged", `,"max_tokens":"17","max_completion_tokens":4096`, `"17"`, "4096"},
		{"boolean_explicit_limit_unchanged", `,"max_tokens":false,"max_completion_tokens":4096`, "false", "4096"},
		{"overflow_explicit_limit_unchanged", `,"max_tokens":9223372036854775808,"max_completion_tokens":4096`, "9223372036854775808", "4096"},
		{"zero_alias_with_explicit_limit_unchanged", `,"max_tokens":17,"max_completion_tokens":0`, "17", "0"},
		{"invalid_alias_with_explicit_limit_unchanged", `,"max_tokens":17,"max_completion_tokens":"4096"`, "17", `"4096"`},
	}
	for _, realm := range []string{"cn", "global"} {
		for _, tc := range cases {
			t.Run(realm+"/"+tc.name, func(t *testing.T) {
				const metadata = `{"exact":9007199254740993,"negative_zero":-0}`
				source := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Return OK."}],"metadata":` + metadata + tc.fields + `}`
				body := []byte(source)
				client := New()
				prepared := client.prepareBody(body, realm, "budget-fixture", "budget-conversation")
				var sent map[string]json.RawMessage
				if err := json.Unmarshal(prepared, &sent); err != nil {
					t.Fatal(err)
				}
				for field, want := range map[string]string{"max_tokens": tc.wantLimit, "max_completion_tokens": tc.wantAlias} {
					if got := string(sent[field]); got != want {
						t.Errorf("%s=%s; want %s (empty means absent)", field, got, want)
					}
				}
				if got := string(sent["metadata"]); got != metadata {
					t.Errorf("unrelated numeric metadata changed: %s", got)
				}
				if string(body) != source {
					t.Error("outbound adaptation changed original caller request")
				}
			})
		}
	}
}

func TestChatStreamMaxCompletionTokensReachesUpstreamBudget(t *testing.T) {
	for _, fields := range []string{
		`,"max_completion_tokens":1`,
		`,"max_tokens":null,"max_completion_tokens":1`,
		`,"max_tokens":1,"max_completion_tokens":4096`,
	} {
		t.Run(fields, func(t *testing.T) {
			client := testClient(func(request *http.Request) (*http.Response, error) {
				var sent map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				// This fixture only accepts WorkBuddy's actual output-budget field.
				// Sending the alias alone would leave the caller's budget unapplied.
				if string(sent["max_tokens"]) != "1" {
					return jsonResp(http.StatusBadRequest, `{"code":11101,"msg":"fixture requires max_tokens=1"}`), nil
				}
				if _, exists := sent["max_completion_tokens"]; exists {
					t.Error("consumed output-budget alias still reached upstream")
				}
				frames := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n" +
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":1,\"total_tokens\":8}}\n\n" +
					"data: [DONE]\n\n"
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}, nil
			})
			source := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Return OK."}]` + fields + `}`
			reader, status, errorBody, err := client.ChatStreamContext(context.Background(), &auth.Auth{UID: "budget-fixture"}, []byte(source), "", ChatMeta{})
			if err != nil || status != http.StatusOK {
				t.Fatalf("output budget was not accepted upstream: status=%d body=%s err=%v", status, errorBody, err)
			}
			defer reader.Close()
			response, err := Aggregate(reader)
			if err != nil {
				t.Fatal(err)
			}
			usage, _ := response["usage"].(map[string]any)
			if output, ok := UsageCount(usage["completion_tokens"]); !ok || output != 1 {
				t.Errorf("measured output changed after budget alias mapping: %#v", usage)
			}
		})
	}
}
