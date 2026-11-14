package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegressionK6ProductionLegacyIdentity(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	captured := make(chan map[string]any, 4)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case BatchCatalogPath:
			io.WriteString(w, maxTierFixture)
		case ChatPath:
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return
			}
			captured <- request
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":2,\"cache_read_input_tokens\":0}\n\nevent: done\ndata: {}\n\n")
		case SessionUsagePath:
			var request map[string]any
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"user_usage_group_by_session": map[string]any{
				"session_id": request["session_id"], "model_name": "GLM-5.3", "credits_float": 0.06,
				"extra_info": map[string]any{"input_token": 100, "output_token": 2, "cache_read_token": 80, "cache_write_token": 0},
			}})
		default:
			t.Errorf("unexpected local fixture path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer origin.Close()
	oldGateway := GatewayBaseVar
	GatewayBaseVar = origin.URL
	defer func() { GatewayBaseVar = oldGateway; InvalidateCatalog("") }()
	InvalidateCatalog("")
	for _, model := range []string{"glm-5.3-dev", "glm-5.3_dev", "glm-5.3__dev"} {
		t.Run(model, func(t *testing.T) {
			prepared, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}})
			r, status, _, err := ChatStream(context.Background(), testAuth(), prepared, "")
			if err != nil || status != 200 {
				t.Fatalf("fixture ChatStream failed: status=%d err=%v", status, err)
			}
			defer r.Close()
			output, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			wire := <-captured
			if wire["model"] != "glm-5.3__dev" || wire["function"] != "solo_work_lite" {
				t.Fatalf("fixture did not select same outbound model: model=%v function=%v", wire["model"], wire["function"])
			}
			t.Logf("request=%s outbound_model=%v function=%v", model, wire["model"], wire["function"])
			if !strings.Contains(string(output), `"cached_tokens":80`) {
				t.Fatalf("real ChatStream path failed cache reconciliation for request=%s; outbound model was verified", model)
			}
		})
	}
}

func TestRegressionK12PartialSnapshotPreservesEarlierObservation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "adjacent_control"
		if partial {
			name = "intervening_partial_usage"
		}
		t.Run(name, func(t *testing.T) {
			events := reviewUsageEvent(`{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":8}`)
			if partial {
				events += reviewUsageEvent(`{"reasoning_tokens":1}`)
			}
			events += reviewUsageEvent(`{"prompt_tokens":100,"completion_tokens":2,"cache_read_input_tokens":0}`)
			bill := matchedCacheBill(100, 2, 10, 0)
			u := reviewFinalUsage(t, "deepseek-v4.1-flash", events, &bill)
			if read := reviewCacheRead(u); read != 8 {
				t.Fatalf("earlier observed positive cache replaced: got=%.0f want=8 usage=%v", read, u)
			}
		})
	}
}

func TestRegressionK14RemoteFinalSummaryEndToEnd(t *testing.T) {
	for _, end := range []struct{ name, wire string }{
		{"truncated_eof", ""},
		{"explicit_done", "event: done\ndata: {\"status\":\"completed\"}\n\n"},
		{"done_sentinel", "data: [DONE]\n\n"},
	} {
		t.Run(end.name, func(t *testing.T) {
			wire := "event: plan_item\ndata: {\"tool_call_info\":{\"id\":\"fixture-finish\",\"name\":\"finish\",\"params\":{\"summary\":\"fixture-final-answer\"}}}\n\n"
			wire += end.wire
			converted := translateRemoteEvents(io.NopCloser(strings.NewReader(wire)), "glm-5.3", "fixture-session")
			output := translateStreamWithCredit(converted, "glm-5.3", nil, nil)
			defer output.Close()
			data, err := io.ReadAll(output)
			if end.wire == "" {
				if err == nil || !strings.Contains(err.Error(), "terminal") || strings.Contains(string(data), "data: [DONE]") || strings.Contains(string(data), `"finish_reason":"stop"`) {
					t.Fatal("EOF without a terminal must remain truncated")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "fixture-final-answer") != 1 || strings.Count(string(data), "data: [DONE]") != 1 {
				t.Fatal("summary or terminal duplicated/lost")
			}
			if !strings.Contains(string(data), "fixture-final-answer") {
				t.Fatal("finish summary lost after complete Remote converter -> OpenAI translator chain")
			}
		})
	}
}

func TestLegacyBudgetAtActualRequestBoundary(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	for _, tc := range []struct {
		name, fields string
		want         int64
		present      bool
	}{
		{"modern", `"max_completion_tokens":1`, 1, true},
		{"legacy-priority", `"max_tokens":2,"max_completion_tokens":1`, 2, true},
		{"null-fallback", `"max_tokens":null,"max_completion_tokens":1`, 1, true},
		{"invalid-primary", `"max_tokens":-1,"max_completion_tokens":1`, 0, false},
		{"zero", `"max_completion_tokens":0`, 0, false},
		{"fraction", `"max_completion_tokens":1.000000000000001`, 0, false},
		{"exact-exponent", `"max_completion_tokens":1e2`, 100, true},
		{"overflow", `"max_completion_tokens":9223372036854775808`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan json.Number, 1)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != ChatPath {
					io.WriteString(w, `{}`)
					return
				}
				var obj map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if decoder.Decode(&obj) != nil {
					t.Error("fixture decode failed")
					return
				}
				budget, _ := obj["max_tokens"].(json.Number)
				captured <- budget
				io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: done\ndata: {}\n\n")
			}))
			defer origin.Close()
			old := GatewayBaseVar
			GatewayBaseVar = origin.URL
			InvalidateCatalog("")
			defer func() { GatewayBaseVar = old; InvalidateCatalog("") }()
			request := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"fixture"}],` + tc.fields + `}`)
			stream, status, _, err := ChatStream(context.Background(), testAuth(), request, "")
			if err != nil || status != 200 {
				t.Fatal("actual request failed")
			}
			_, err = io.Copy(io.Discard, stream)
			stream.Close()
			if err != nil {
				t.Fatal(err)
			}
			got := <-captured
			count, valid := usageCount(got)
			if (got != "") != tc.present || tc.present && (!valid || count != tc.want) {
				t.Fatalf("budget present=%t count=%d", got != "", count)
			}
		})
	}
}

func TestRegressionK15LegacyCompletionBudget(t *testing.T) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(key, func(t *testing.T) {
			prepared, _ := json.Marshal(map[string]any{"model": "glm-5.3", "messages": []any{map[string]any{"role": "user", "content": "fixture"}}, key: 1})
			body, err := buildChatBodyWithSession(prepared, "fixture-request", "fixture-session", nil)
			if err != nil {
				t.Fatal(err)
			}
			var parsed map[string]any
			if err = json.Unmarshal(body, &parsed); err != nil {
				t.Fatal(err)
			}
			if parsed["max_tokens"] != float64(1) {
				t.Fatalf("outbound budget missing: input_field=%s max_tokens=%v", key, parsed["max_tokens"])
			}
		})
	}
}
