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

const billingSelectionCatalog = `{"function_configs":[
 {"function":"solo_work_lite","config_info_list":[
  {"config_name":"catalog-alias","usage":"chat_completion","model_detail_list":[{"model_name":"Legacy-Target__dev"}]},
  {"config_name":"glm-5.3","usage":"chat_completion","model_detail_list":[{"model_name":"glm-5.3__dev"}]},
  {"config_name":"Doubao-Seed-2.1-Pro","usage":"chat_completion","model_detail_list":[{"model_name":"Doubao-Seed-2.1-Pro__dev"}]},
  {"config_name":"qwen3.8-max","usage":"chat_completion","model_detail_list":[{"model_name":"qwen3.8-max__dev"}]}]},
 {"function":"solo_agent_remote","config_info_list":[
  {"config_name":"catalog-alias","usage":"chat_completion","model_detail_list":[{"model_name":"Remote-Target__dev"}]},
  {"config_name":"glm-5.3","usage":"chat_completion","model_detail_list":[{"model_name":"glm-5.3__dev"}]},
  {"config_name":"Doubao-Seed-2.1-Pro","usage":"chat_completion","model_detail_list":[{"model_name":"Doubao-Seed-2.1-Pro__dev"}]},
  {"config_name":"qwen3.8-max","usage":"chat_completion","model_detail_list":[{"model_name":"qwen3.8-max__dev"}]}]}
]}`

// Exercise request selection and billing together; a translator-only fixture
// cannot prove that the selected function's identity was passed down.
func TestRegressionK6SelectedOutboundIdentity(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	for _, route := range []string{"legacy", "resend", "remote"} {
		for _, tc := range []struct {
			name, model, target, bill string
			noCatalog                 bool
			wantCache                 bool
		}{
			{"dev-alias", "glm-5.3_dev", "glm-5.3", "GLM-5.3", false, true},
			{"seed-alias", "seed-2.1-pro", "Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Pro", false, true},
			{"seed-offline", "seed-2.1-pro", "Doubao-Seed-2.1-Pro", "Doubao-Seed-2.1-Pro", true, true},
			{"function-profile", "catalog-alias", "", "", false, true},
			{"reject-source-alias", "catalog-alias", "", "catalog-alias", false, false},
			{"literal-max-name", "qwen3.8-max", "qwen3.8-max", "qwen3.8-max", false, true},
			{"reject-stripped-max", "qwen3.8-max", "qwen3.8-max", "qwen3.8", false, false},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				function := "solo_work_lite"
				target := tc.target
				if target == "" {
					target = "Legacy-Target"
				}
				if route == "remote" {
					function = "solo_agent_remote"
					if tc.target == "" {
						target = "Remote-Target"
					}
				}
				billModel := tc.bill
				if billModel == "" {
					billModel = target
				}
				wireModel := target + "__dev"
				if tc.noCatalog {
					wireModel = target
				}
				captured := make(chan map[string]any, 1)
				billed := make(chan string, 1)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == BatchCatalogPath:
						if tc.noCatalog {
							io.WriteString(w, `{"function_configs":[]}`)
						} else {
							io.WriteString(w, billingSelectionCatalog)
						}
					case r.URL.Path == SessionUsagePath:
						var request map[string]any
						_ = json.NewDecoder(r.Body).Decode(&request)
						id, _ := request["session_id"].(string)
						billed <- id
						_ = json.NewEncoder(w).Encode(map[string]any{"user_usage_group_by_session": map[string]any{
							"session_id": id, "model_name": billModel, "credits_float": 0.06,
							"extra_info": map[string]any{"input_token": 100, "output_token": 2, "cache_read_token": 80, "cache_write_token": 0},
						}})
					case r.URL.Path == ChatPath:
						var request map[string]any
						_ = json.NewDecoder(r.Body).Decode(&request)
						captured <- request
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":2,\"cache_read_input_tokens\":0}\n\nevent: done\ndata: {}\n\n")
					case r.URL.Path == "/chat_sessions":
						var request map[string]any
						_ = json.NewDecoder(r.Body).Decode(&request)
						initial, _ := request["initial_message"].(map[string]any)
						captured <- initial
						io.WriteString(w, `{"code":0,"data":{"chat_session_id":"fixture-session","message_id":"fixture-user-message"}}`)
					case strings.HasSuffix(r.URL.Path, "/events"):
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "event: message\ndata: {\"content\":\"fixture\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":100,\"completion_tokens\":2,\"cache_read_input_tokens\":0}\n\nevent: done\ndata: {}\n\n")
					case strings.HasSuffix(r.URL.Path, "/stop"):
						io.WriteString(w, `{}`)
					default:
						t.Error("unexpected fixture endpoint")
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer origin.Close()
				oldGateway, oldRemote := GatewayBaseVar, RemoteBaseVar
				GatewayBaseVar, RemoteBaseVar = origin.URL, origin.URL
				InvalidateCatalog("")
				defer func() { GatewayBaseVar, RemoteBaseVar = oldGateway, oldRemote; InvalidateCatalog("") }()
				prepared, _ := json.Marshal(map[string]any{"model": tc.model, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}})
				var stream io.ReadCloser
				var err error
				switch route {
				case "remote":
					stream, err = ChatStreamRemote(context.Background(), testAuth(), prepared, "")
				case "resend":
					stream, err = (&remoteFallbackReader{ctx: context.Background(), auth: testAuth()}).resend(prepared)
				default:
					var status int
					stream, status, _, err = ChatStream(context.Background(), testAuth(), prepared, "")
					if status != 200 {
						t.Fatalf("status=%d", status)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				result, err := io.ReadAll(stream)
				stream.Close()
				if err != nil {
					t.Fatal(err)
				}
				wire := <-captured
				wireFunction, actualModel := wire["function"], wire["model"]
				if route == "remote" {
					wireFunction, actualModel = wire["agent_type"], wire["model_name"]
				}
				if wireFunction != function || actualModel != wireModel {
					t.Fatalf("wrong selected fixture identity: function=%v model=%v", wireFunction, actualModel)
				}
				billID := <-billed
				if route == "remote" {
					if billID != "fixture-user-message" {
						t.Fatal("remote queried wrong billing key")
					}
				} else if billID != wire["session_id"] {
					t.Fatal("legacy queried wrong billing key")
				}
				if strings.Contains(string(result), `"cached_tokens":80`) != tc.wantCache {
					t.Fatal("cache attribution did not follow actual outbound identity")
				}
			})
		}
	}
}

func TestBillingSelectionRequiresExplicitIdentity(t *testing.T) {
	tr := newTranslator("deepseek-v4.1-flash")
	tr.recordUsage(map[string]any{"prompt_tokens": 100, "completion_tokens": 2, "cache_read_input_tokens": 0})
	tr.reconcileBilledCache(matchedCacheBill(100, 2, 80, 0))
	if tr.usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != int64(0) {
		t.Fatal("request alias substituted for missing outbound identity")
	}
}
