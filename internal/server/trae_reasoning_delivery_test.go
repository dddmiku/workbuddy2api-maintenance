package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/upstream/trae"
)

// The origin cannot send content until the actual HTTP/2 client sees reasoning.
// This exercises the handler's real StreamOptions, Trae translation, protocol
// conversion and common loop guard; testing the translator alone misses the
// second buffer between translated reasoning and the downstream client.
func TestTraeReasoningReachesClientBeforeOriginContent(t *testing.T) {
	testTraeProgressReachesClientBeforeOriginCompletion(t, false, false)
}

func TestTraeContentReachesClientBeforeOriginCompletion(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		name := "silence"
		if heartbeat {
			name = "heartbeat"
		}
		t.Run(name, func(t *testing.T) {
			testTraeProgressReachesClientBeforeOriginCompletion(t, true, heartbeat)
		})
	}
}

func testTraeProgressReachesClientBeforeOriginCompletion(t *testing.T, contentOnly, heartbeat bool) {
	t.Helper()
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	for _, protocol := range []struct {
		name, path string
	}{
		{"chat", "/v1/chat/completions"},
		{"responses", "/v1/responses"},
		{"messages", "/v1/messages"},
	} {
		for _, mode := range []struct {
			name            string
			guard, stopOnly bool
		}{
			{"retry", true, false},
			{"stop_only", true, true},
			{"guard_disabled", false, false},
		} {
			t.Run(protocol.name+"/"+mode.name, func(t *testing.T) {
				started := time.Now()
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseOrigin := func() { releaseOnce.Do(func() { close(release) }) }
				var forcedRelease atomic.Bool
				var originCalls atomic.Int32
				var originHeartbeats atomic.Int32
				var originContentNS atomic.Int64
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != trae.ChatPath {
						io.WriteString(w, "{}")
						return
					}
					originCalls.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if contentOnly {
						io.WriteString(w, "event: output\ndata: {\"response\":\"OK\"}\n\n")
					} else {
						io.WriteString(w, "event: output\ndata: {\"reasoning_content\":\"Inspect the fixture, then report the result.\"}\n\n")
					}
					if heartbeat {
						io.WriteString(w, ": keepalive\n\n")
						originHeartbeats.Add(1)
					}
					w.(http.Flusher).Flush()
					// This timeout makes the old implementation terminate with a
					// useful failure, rather than deadlocking behind the buffer.
					timer := time.NewTimer(600 * time.Millisecond)
					defer timer.Stop()
					var beats <-chan time.Time
					if heartbeat {
						ticker := time.NewTicker(20 * time.Millisecond)
						defer ticker.Stop()
						beats = ticker.C
					}
				waitForDelivery:
					for {
						select {
						case <-release:
							break waitForDelivery
						case <-timer.C:
							forcedRelease.Store(true)
							break waitForDelivery
						case <-beats:
							io.WriteString(w, ": keepalive\n\n")
							originHeartbeats.Add(1)
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
					originContentNS.Store(time.Since(started).Nanoseconds())
					if !contentOnly {
						io.WriteString(w, "event: output\ndata: {\"response\":\"fixture complete\"}\n\n")
					}
					io.WriteString(w, "event: token_usage\ndata: {\"prompt_tokens\":20,\"completion_tokens\":10}\n\nevent: done\ndata: {}\n\n")
				}))
				oldGateway := trae.GatewayBaseVar
				trae.GatewayBaseVar = origin.URL
				defer func() { trae.GatewayBaseVar = oldGateway }()
				defer origin.Close()
				acct, err := auth.Parse([]byte(`{"auth":{"realm":"trae","accessToken":"fixture","expiresAt":9999999999},"account":{"uid":"delivery-fixture"}}`))
				if err != nil {
					t.Fatal("fixture account was invalid")
				}
				h := NewHandler(Config{Pool: testPoolWith(acct), Upstream: upstream.New(), ReasoningLoopGuard: &mode.guard})
				h.SetReasoningLoopStopOnly(mode.stopOnly)
				var timingMu sync.Mutex
				timings := make(map[string]time.Duration)
				local := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := trae.WithTimingObserver(r.Context(), func(e trae.TimingEvent) {
						if e.Stage != "upstream_reasoning" && e.Stage != "first_reasoning" && e.Stage != "upstream_content" && e.Stage != "first_content" {
							return
						}
						timingMu.Lock()
						if _, exists := timings[e.Stage]; !exists {
							timings[e.Stage] = time.Since(started)
						}
						timingMu.Unlock()
					})
					h.ServeHTTP(w, r.WithContext(ctx))
				}))
				local.EnableHTTP2 = true
				local.StartTLS()
				defer local.Close()
				// Unblock the origin before closing either server, including
				// early failures before any client response is available.
				defer releaseOrigin()
				client := local.Client()
				client.Timeout = 5 * time.Second
				body := `{"model":"trae:deepseek-v4.1-flash","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"fixture"}]}`
				if protocol.name == "responses" {
					body = `{"model":"trae:deepseek-v4.1-flash","stream":true,"max_output_tokens":128,"input":"fixture"}`
				}
				resp, err := client.Post(local.URL+protocol.path, "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatalf("fixture request failed: error_type=%T", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
					t.Fatalf("fixture response status=%d protocol_major=%d", resp.StatusCode, resp.ProtoMajor)
				}
				var clientProgress time.Duration
				terminalCount := 0
				scanner := bufio.NewScanner(resp.Body)
				for scanner.Scan() {
					line := scanner.Text()
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					reasoning, content, terminal := traeDeliveryFrame(protocol.name, data)
					progress := reasoning
					if contentOnly {
						progress = content
					}
					if progress && clientProgress == 0 {
						clientProgress = time.Since(started)
						releaseOrigin()
					}
					if terminal {
						terminalCount++
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatalf("fixture stream failed: error_type=%T", err)
				}
				timingMu.Lock()
				raw, translated := timings["upstream_reasoning"], timings["first_reasoning"]
				if contentOnly {
					raw, translated = timings["upstream_content"], timings["first_content"]
				}
				timingMu.Unlock()
				content := time.Duration(originContentNS.Load())
				t.Logf("guard=%t retry_available=%t raw_ms=%.3f translated_ms=%.3f client_ms=%.3f origin_release_ms=%.3f delivery_wait_ms=%.3f forced_release=%t calls=%d terminal_count=%d origin_heartbeats=%d",
					mode.guard, mode.guard && !mode.stopOnly, float64(raw.Microseconds())/1000,
					float64(translated.Microseconds())/1000, float64(clientProgress.Microseconds())/1000,
					float64(content.Microseconds())/1000, float64((clientProgress-translated).Microseconds())/1000,
					forcedRelease.Load(), originCalls.Load(), terminalCount, originHeartbeats.Load())
				if raw == 0 || translated == 0 || clientProgress == 0 || terminalCount != 1 || originCalls.Load() != 1 || (heartbeat && originHeartbeats.Load() == 0) {
					t.Fatal("fixture did not exercise one complete translated progress stream")
				}
				// Windows may report equal timestamps within one clock tick;
				// the release channel proves the ordering independently.
				if forcedRelease.Load() || clientProgress > content {
					t.Error("normal progress reached the client only after the origin was forcibly released")
				}
			})
		}
	}
}

func traeDeliveryFrame(protocol, data string) (reasoning, content, terminal bool) {
	if protocol == "chat" && data == "[DONE]" {
		return false, false, true
	}
	var frame struct {
		Type    string          `json:"type"`
		Delta   json.RawMessage `json:"delta"`
		Choices []struct {
			Delta struct {
				Reasoning string `json:"reasoning_content"`
				Content   string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &frame) != nil {
		return false, false, false
	}
	switch protocol {
	case "chat":
		for _, choice := range frame.Choices {
			reasoning = reasoning || choice.Delta.Reasoning != ""
			content = content || choice.Delta.Content != ""
		}
		return reasoning, content, false
	case "responses":
		var delta string
		if frame.Type == "response.reasoning_summary_text.delta" || frame.Type == "response.output_text.delta" {
			_ = json.Unmarshal(frame.Delta, &delta)
		}
		return frame.Type == "response.reasoning_summary_text.delta" && delta != "",
			frame.Type == "response.output_text.delta" && delta != "", frame.Type == "response.completed"
	case "messages":
		var delta struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
			Text     string `json:"text"`
		}
		if frame.Type == "content_block_delta" {
			_ = json.Unmarshal(frame.Delta, &delta)
		}
		return delta.Type == "thinking_delta" && delta.Thinking != "",
			delta.Type == "text_delta" && delta.Text != "", frame.Type == "message_stop"
	}
	return false, false, false
}

// Once a valid reasoning delta has reached the real client, a later loop must
// stop the same request without replaying earlier output or leaking its trigger.
func TestTraeDeliveredReasoningStillStopsLoop(t *testing.T) {
	t.Setenv("WB2API_TRAE_REMOTE", "0")
	for _, protocol := range []struct {
		name, path string
	}{
		{"chat", "/v1/chat/completions"},
		{"responses", "/v1/responses"},
		{"messages", "/v1/messages"},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			started := time.Now()
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseOrigin := func() { releaseOnce.Do(func() { close(release) }) }
			var forcedRelease atomic.Bool
			var originCalls atomic.Int32
			const firstReasoning = "Inspect the fixture, then report the result.\n"
			loopFrame, _ := json.Marshal(map[string]any{"reasoning_content": firstReasoning + reasoningGuardLines(400)})
			initialFrame, _ := json.Marshal(map[string]any{"reasoning_content": firstReasoning})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != trae.ChatPath {
					io.WriteString(w, "{}")
					return
				}
				originCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: output\ndata: "+string(initialFrame)+"\n\n")
				w.(http.Flusher).Flush()
				timer := time.NewTimer(600 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-release:
				case <-timer.C:
					forcedRelease.Store(true)
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, "event: output\ndata: "+string(loopFrame)+"\n\nevent: output\ndata: {\"response\":\"after-loop-forbidden\"}\n\nevent: done\ndata: {}\n\n")
			}))
			defer origin.Close()
			oldGateway := trae.GatewayBaseVar
			trae.GatewayBaseVar = origin.URL
			defer func() { trae.GatewayBaseVar = oldGateway }()
			acct, err := auth.Parse([]byte(`{"auth":{"realm":"trae","accessToken":"fixture","expiresAt":9999999999},"account":{"uid":"delivery-loop-fixture"}}`))
			if err != nil {
				t.Fatal("fixture account was invalid")
			}
			guard := true
			h := NewHandler(Config{Pool: testPoolWith(acct), Upstream: upstream.New(), ReasoningLoopGuard: &guard})
			h.SetReasoningLoopStopOnly(false)
			local := httptest.NewUnstartedServer(h)
			local.EnableHTTP2 = true
			local.StartTLS()
			defer local.Close()
			defer releaseOrigin()
			client := local.Client()
			client.Timeout = 5 * time.Second
			body := `{"model":"trae:deepseek-v4.1-flash","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"fixture"}]}`
			if protocol.name == "responses" {
				body = `{"model":"trae:deepseek-v4.1-flash","stream":true,"max_output_tokens":128,"input":"fixture"}`
			}
			resp, err := client.Post(local.URL+protocol.path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("fixture request failed: error_type=%T", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
				t.Fatalf("fixture response status=%d protocol_major=%d", resp.StatusCode, resp.ProtoMajor)
			}
			var clientReasoning time.Duration
			var received strings.Builder
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				received.WriteString(line)
				received.WriteByte('\n')
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				reasoning, _, _ := traeDeliveryFrame(protocol.name, data)
				if reasoning && clientReasoning == 0 {
					clientReasoning = time.Since(started)
					releaseOrigin()
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("fixture stream failed: error_type=%T", err)
			}
			output := received.String()
			errorCount := strings.Count(output, `"code":"upstream_reasoning_loop"`)
			loopLeaked := strings.Contains(output, "checking the same step once more")
			t.Logf("client_reasoning_ms=%.3f forced_release=%t calls=%d error_count=%d loop_leaked=%t",
				float64(clientReasoning.Microseconds())/1000, forcedRelease.Load(), originCalls.Load(), errorCount, loopLeaked)
			if clientReasoning == 0 || forcedRelease.Load() || originCalls.Load() != 1 || errorCount != 1 || loopLeaked || strings.Contains(output, "after-loop-forbidden") {
				t.Fatal("delivered reasoning did not stop a subsequent loop without retry or trigger leakage")
			}
			switch protocol.name {
			case "chat":
				if strings.Count(output, "data: [DONE]") != 1 || strings.Contains(output, `"finish_reason":"stop"`) {
					t.Fatal("Chat loop failure did not terminate exactly once")
				}
			case "responses":
				if strings.Count(output, "event: response.failed\n") != 1 || strings.Contains(output, "event: response.completed\n") {
					t.Fatal("Responses loop failure did not terminate exactly once")
				}
			case "messages":
				if strings.Count(output, "event: error\n") != 1 || strings.Contains(output, "event: message_stop\n") {
					t.Fatal("Messages loop failure did not terminate exactly once")
				}
			}
		})
	}
}
