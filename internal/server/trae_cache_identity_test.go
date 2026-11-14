package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
	"workbuddy2api/internal/usage"
)

func TestTraeCallerAndHeaderSessionOnWire(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	captured := make(chan map[string]any, 16)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != trae.ChatPath {
			io.WriteString(w, "{}")
			return
		}
		var b map[string]any
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			w.WriteHeader(400)
			return
		}
		captured <- b
		io.WriteString(w, "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: done\ndata: {}\n\n")
	}))
	defer origin.Close()
	old := trae.GatewayBaseVar
	trae.GatewayBaseVar = origin.URL
	defer func() { trae.GatewayBaseVar = old }()
	acct, err := auth.Parse([]byte(`{"auth":{"realm":"trae","accessToken":"fixture","expiresAt":9999999999},"account":{"uid":"caller-session-fixture"}}`))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := apikeys.Open(filepath.Join(t.TempDir(), "keys.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, keyA, err := keys.Create("caller-a", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, keyB, err := keys.Create("caller-b", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: testPoolWith(acct), Upstream: upstream.New(), APIKeys: keys})
	send := func(key, header, bodyConv string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"model": "trae:deepseek-v4.1-flash", "stream": true, "conversation_id": bodyConv, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Conversation-ID", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal("fixture handler failed")
		}
		return <-captured
	}
	a := send(keyA, "header-a", "")
	b := send(keyA, "header-a", "")
	if a["session_id"] == b["session_id"] {
		t.Error("same conversation reused a per-request billing session")
	}
	if a["request_id"] == b["request_id"] {
		t.Error("turn request identity was reused")
	}
	if c := send(keyA, "header-b", ""); c["session_id"] == a["session_id"] {
		t.Error("different header conversations shared session")
	}
	if c := send(keyB, "header-a", ""); c["session_id"] == a["session_id"] {
		t.Error("authenticated callers shared session")
	}
	if c := send(keyA, "header-a", "body-other"); c["session_id"] == a["session_id"] {
		t.Error("another turn in the same header conversation reused its bill")
	}
	if c := send(keyA, "", "body-conversation"); c["session_id"] == a["session_id"] {
		t.Error("body conversation collided with header conversation")
	}
}

func TestTraeHeaderPrecedenceAndCallerIdentity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Conversation-ID", " header-a ")
	body := []byte(`{"conversation_id":"body-a"}`)
	caller, conversation := resolveTraeIdentity(r, "caller-fixture-a", "", body)
	if caller != "caller-fixture-a" || conversation == "" {
		t.Fatal("authenticated identity was lost")
	}
	_, overridden := resolveTraeIdentity(r, "caller-fixture-a", "", []byte(`{"conversation_id":"body-b"}`))
	if conversation != overridden {
		t.Fatal("body identity overrode the header conversation")
	}
	_, otherCaller := resolveTraeIdentity(r, "caller-fixture-b", "", body)
	if conversation == otherCaller {
		t.Fatal("conversation scoping crossed callers")
	}
	r.Header.Del("X-Conversation-ID")
	_, bodyConversation := resolveTraeIdentity(r, "caller-fixture-a", "", body)
	if bodyConversation == "" || bodyConversation == conversation {
		t.Fatal("body fallback did not select its own conversation")
	}
	legacy, _ := resolveTraeIdentity(r, "", "fixture-legacy-key", body)
	if legacy == "" || legacy == "fixture-legacy-key" {
		t.Fatal("legacy authentication identity was absent or exposed the secret")
	}
}

func TestTraeUsagePresenceThroughStatsAndLedger(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		cached       int
	}{
		{"missing", "", -1},
		{"explicit_zero", `,"cache_read_input_tokens":0`, 0},
		{"flat_hit", `,"cache_read_input_tokens":8`, 8},
		{"nested_hit", `,"prompt_tokens_details":{"cached_tokens":8}`, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := "event: output\ndata: {\"response\":\"fixture\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":10,\"completion_tokens\":2" + tc.fields + "}\n\nevent: done\ndata: {}\n\n"
			stream := trae.TranslateStream(io.NopCloser(strings.NewReader(events)), "deepseek-v4.1-flash")
			defer stream.Close()
			stats := newChatStatsReaderSince(stream, time.Now())
			if _, err := io.Copy(io.Discard, stats); err != nil {
				t.Fatal("fixture translation failed")
			}
			if got := stats.CachedTokens(); got != tc.cached {
				t.Fatalf("cache observation changed: got=%d want=%d", got, tc.cached)
			}
			st := newChatStat(time.Now(), nil, true)
			st.absorbUsage(stats)
			if st.cached != tc.cached {
				t.Error("chatStat changed cache presence")
			}
			trace := &requestTrace{enabled: true}
			trace.event(upstream.ChatAttemptEvent{Stage: "start", At: time.Now()})
			trace.observe(stats, "")
			attempt := trace.lastAttempt()
			if attempt == nil {
				t.Fatal("missing trace attempt")
			}
			if tc.cached < 0 {
				if attempt.CachedTokens != nil {
					t.Error("missing cache became known zero in request history")
				}
			} else if attempt.CachedTokens == nil || *attempt.CachedTokens != int64(tc.cached) {
				t.Error("request history changed cache count")
			}
			ledger, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			ledger.RecordOutcome("fixture", "fixture", "fixture", "trae:deepseek-v4.1-flash", st.prompt, st.toks, st.cached, 0, false, usage.Outcome{}, time.Now())
			if got := ledger.Snapshot().Totals.CachedTokens; got != int64(max(0, tc.cached)) {
				t.Error("ledger aggregated an unobserved cache hit")
			}
		})
	}
}
