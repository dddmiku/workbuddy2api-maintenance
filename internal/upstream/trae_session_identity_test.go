package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream/trae"
)

func TestTraeSessionIdentityOnWire(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	bodies := make(chan map[string]any, 16)
	billIDs := make(chan string, 16)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == trae.SessionUsagePath {
			var query struct {
				SessionID string `json:"session_id"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&query) != nil || query.SessionID == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			billIDs <- query.SessionID
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"user_usage_group_by_session": map[string]any{
				"session_id": query.SessionID, "model_name": "fixture-model", "credits_float": 0.1,
				"extra_info": map[string]any{"input_token": 1, "output_token": 1},
			}})
			return
		}
		if r.URL.Path != trae.ChatPath {
			io.WriteString(w, "{}")
			return
		}
		var b map[string]any
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			w.WriteHeader(400)
			return
		}
		bodies <- b
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":1,\"completion_tokens\":1,\"cache_read_input_tokens\":0}\n\nevent: done\ndata: {}\n\n")
	}))
	defer origin.Close()
	old := trae.GatewayBaseVar
	trae.GatewayBaseVar = origin.URL
	defer func() { trae.GatewayBaseVar = old }()
	client := New()
	send := func(caller, conv, account, text string) map[string]any {
		t.Helper()
		a, err := auth.Parse([]byte(`{"auth":{"accessToken":"fixture","realm":"trae","expiresAt":9999999999},"account":{"uid":"` + account + `"}}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": text}}})
		rc, status, _, err := client.ChatStreamContext(context.Background(), a, body, "", ChatMeta{CallerID: caller, ConversationID: conv})
		if err != nil || status != 200 {
			t.Fatal("fixture upstream call failed")
		}
		_, err = io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			t.Fatal("fixture stream failed")
		}
		posted := <-bodies
		select {
		case billedID := <-billIDs:
			if billedID != posted["session_id"] {
				t.Error("bill lookup did not use this turn's posted session_id")
			}
		default:
			t.Fatal("completed turn did not look up its bill")
		}
		if len(billIDs) != 0 {
			t.Error("a single completed turn queried multiple billing identities")
		}
		return posted
	}
	a := send("caller-a", "conversation-a", "session-fixture-a", "first")
	b := send("caller-a", "conversation-a", "session-fixture-a", "later")
	if a["request_id"] == b["request_id"] {
		t.Error("request_id was reused across turns")
	}
	if a["session_id"] == b["session_id"] {
		t.Error("same conversation reused a billing session_id across turns")
	}
	for name, c := range map[string]map[string]any{
		"conversation": send("caller-a", "conversation-b", "session-fixture-a", "first"),
		"caller":       send("caller-b", "conversation-a", "session-fixture-a", "first"),
		"account":      send("caller-a", "conversation-a", "session-fixture-b", "first"),
	} {
		if a["session_id"] == c["session_id"] {
			t.Errorf("session_id crossed %s boundary", name)
		}
	}
	c := send("caller-a", "", "session-fixture-a", "shared system and user")
	d := send("caller-a", "", "session-fixture-a", "shared system and user")
	if c["session_id"] == d["session_id"] {
		t.Error("requests without conversation identity shared a session")
	}
	c = send("", "untrusted-conversation", "session-fixture-a", "first")
	d = send("", "untrusted-conversation", "session-fixture-a", "first")
	if c["session_id"] == d["session_id"] {
		t.Error("requests without authenticated caller identity shared a session")
	}
	if strings.Contains(a["session_id"].(string), "conversation-a") {
		t.Error("session_id exposed source identity")
	}
}
