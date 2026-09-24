// ═══ 更新日志 ═══
// 2026-09-25：回归完整历史成功请求的分帧用量；流式与聚合均保留上游累计总数、缓存和费用。
package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

func TestSuccessfulHistoryPreservesSplitUsage(t *testing.T) {
	// Counters are cumulative observations, and may arrive separately from
	// the completion and credit frames. The final total must remain 74882.
	const raw = "data: {\"id\":\"history-success\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"History preserved.\"}}],\"usage\":{\"prompt_tokens\":74332,\"prompt_tokens_details\":{\"cached_tokens\":70000}}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":100,\"total_tokens\":74432}}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":550,\"total_tokens\":74882}}\n\n" +
		"data: {\"usage\":{\"credit\":0.25,\"prompt_tokens_details\":{\"audio_tokens\":0}}}\n\n" +
		"data: [DONE]\n\n"
	for _, mode := range []string{"aggregate", "stream"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan []byte, 16)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, raw)
			}))
			defer srv.Close()
			client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
			body := contextHistoryBody(t, 8)
			rc, status, errorBody, err := client.ChatStreamContext(context.Background(), &auth.Auth{UID: "usage-fixture"}, body, "", ChatMeta{})
			if err != nil || status != http.StatusOK || rc == nil {
				t.Fatalf("successful upstream request failed: status=%d body=%q err=%v", status, errorBody, err)
			}
			defer rc.Close()
			if got := len(requests); got != 1 {
				t.Fatalf("successful request made %d attempts; want one", got)
			}
			if got, want := contextHistoryMessages(t, <-requests), contextHistoryMessages(t, body); !reflect.DeepEqual(got, want) {
				t.Fatal("successful request lost or changed original history")
			}
			var usage map[string]any
			if mode == "aggregate" {
				response, err := Aggregate(rc)
				if err != nil {
					t.Fatal(err)
				}
				usage, _ = response["usage"].(map[string]any)
			} else {
				rec := httptest.NewRecorder()
				if err := Stream(rec, rc); err != nil {
					t.Fatal(err)
				}
				if got := strings.Count(rec.Body.String(), "data: [DONE]\n\n"); got != 1 {
					t.Fatalf("stream closing delimiter count=%d; want one", got)
				}
				if err := readSSE(strings.NewReader(rec.Body.String()), func(event sseEvent) (bool, error) {
					frame, done, err := decodeSSEEvent(event)
					if err != nil || done {
						return true, err
					}
					if value, ok := frame["usage"].(map[string]any); ok {
						// The final frame itself must carry all observed counters;
						// don't merge in the test and mask lost output fields.
						usage = value
					}
					return false, nil
				}, nil); err != nil {
					t.Fatal(err)
				}
			}
			for key, want := range map[string]int{"prompt_tokens": 74332, "completion_tokens": 550, "total_tokens": 74882} {
				if got, ok := UsageCount(usage[key]); !ok || got != want {
					t.Errorf("%s=%v; want measured upstream value %d", key, usage[key], want)
				}
			}
			if got, ok := CachedInputTokens(usage); !ok || got != 70000 {
				t.Errorf("cached input=%d known=%v; want 70000", got, ok)
			}
			if got, ok := UsageCredit(usage["credit"]); !ok || got != 0.25 {
				t.Errorf("credit=%v known=%v; want 0.25", got, ok)
			}
		})
	}
}
