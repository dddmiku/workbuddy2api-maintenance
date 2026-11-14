package trae

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// TimingEvent contains durations, protocol labels and numeric usage observations,
// never request data, credentials, account identifiers, URLs or model output.
type TimingEvent struct {
	Stage        string   `json:"stage"`
	Route        string   `json:"route"`
	ElapsedMS    float64  `json:"elapsed_ms"`
	DurationMS   float64  `json:"duration_ms"`
	ConnectMS    float64  `json:"connect_ms,omitempty"`
	Reused       *bool    `json:"reused,omitempty"`
	Status       int      `json:"status,omitempty"`
	Failed       bool     `json:"failed,omitempty"`
	EventName    string   `json:"event_name,omitempty"`
	Bytes        int      `json:"bytes,omitempty"`
	Fields       []string `json:"fields,omitempty"`
	InputTokens  *int64   `json:"input_tokens,omitempty"`
	OutputTokens *int64   `json:"output_tokens,omitempty"`
	CachedTokens *int64   `json:"cached_tokens,omitempty"`
}

type timingKey struct{}
type timingRouteKey struct{}
type timingObserver struct {
	mu      sync.Mutex
	start   time.Time
	observe func(TimingEvent)
	first   map[string]bool
	headers map[string]time.Time
	points  map[string]time.Time
}

// WithTimingObserver enables request-local measurement. The callback must be
// safe for concurrent HTTP/translation/writer calls. With no observer the
// ordinary request and translation paths do not parse or log diagnostic data.
func WithTimingObserver(ctx context.Context, observe func(TimingEvent)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, timingKey{}, &timingObserver{start: time.Now(), observe: observe, first: map[string]bool{}, headers: map[string]time.Time{}, points: map[string]time.Time{}})
}

func timingFrom(ctx context.Context) *timingObserver {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(timingKey{}).(*timingObserver)
	return t
}
func timingRoute(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	route, _ := ctx.Value(timingRouteKey{}).(string)
	return route
}
func withTimingRoute(ctx context.Context, route string) context.Context {
	if timingFrom(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, timingRouteKey{}, route)
}
func timingMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func observeSpan(ctx context.Context, stage string, start time.Time) {
	if t := timingFrom(ctx); t != nil && t.observe != nil {
		t.observe(TimingEvent{Stage: stage, Route: timingRoute(ctx), ElapsedMS: timingMS(time.Since(t.start)), DurationMS: timingMS(time.Since(start))})
	}
}
func observeFirst(ctx context.Context, stage string) {
	t := timingFrom(ctx)
	if t == nil || t.observe == nil {
		return
	}
	route := timingRoute(ctx)
	t.mu.Lock()
	key := route + ":" + stage
	if t.first[key] {
		t.mu.Unlock()
		return
	}
	t.first[key] = true
	now := time.Now()
	t.points[key] = now
	base := t.headers[route]
	// These spans measure actual residency in the translator, independently
	// from waiting for headers/events or later protocol buffering and delivery.
	if stage == "first_reasoning" {
		if raw := t.points[route+":upstream_reasoning"]; !raw.IsZero() {
			base = raw
		}
	} else if stage == "first_content" {
		if raw := t.points[route+":upstream_content"]; !raw.IsZero() {
			base = raw
		}
	}
	if base.IsZero() {
		base = t.start
	}
	t.mu.Unlock()
	t.observe(TimingEvent{Stage: stage, Route: route, ElapsedMS: timingMS(now.Sub(t.start)), DurationMS: timingMS(now.Sub(base))})
}

// ObserveDownstreamWrite measures the first actual network writer operation.
// Flushing/client arrival are recorded independently by the diagnostic client.
func ObserveDownstreamWrite(ctx context.Context, start time.Time, n int) {
	t := timingFrom(ctx)
	if t == nil || t.observe == nil || n <= 0 {
		return
	}
	t.mu.Lock()
	if t.first["downstream:write"] {
		t.mu.Unlock()
		return
	}
	t.first["downstream:write"] = true
	t.mu.Unlock()
	t.observe(TimingEvent{Stage: "write", Route: "downstream", ElapsedMS: timingMS(time.Since(t.start)), DurationMS: timingMS(time.Since(start))})
}

func doTimed(ctx context.Context, client *http.Client, req *http.Request, stage string) (*http.Response, error) {
	t := timingFrom(ctx)
	if t == nil || t.observe == nil {
		return client.Do(req)
	}
	start := time.Now()
	var mu sync.Mutex
	var wrote time.Time
	var connect float64
	var reused *bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			v := info.Reused
			reused = &v
			connect = timingMS(time.Since(start))
			mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { mu.Lock(); wrote = time.Now(); mu.Unlock() },
	}
	resp, err := client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	mu.Lock()
	base := wrote
	if base.IsZero() {
		base = start
	}
	e := TimingEvent{Stage: stage, Route: timingRoute(ctx), ElapsedMS: timingMS(time.Since(t.start)), DurationMS: timingMS(time.Since(base)), ConnectMS: connect, Reused: reused, Failed: err != nil}
	mu.Unlock()
	if resp != nil {
		e.Status = resp.StatusCode
	}
	if stage == "header" && err == nil {
		t.mu.Lock()
		t.headers[e.Route] = time.Now()
		t.mu.Unlock()
		resp.Body = &timingBody{ReadCloser: resp.Body, ctx: ctx}
	}
	t.observe(e)
	return resp, err
}

type timingBody struct {
	io.ReadCloser
	ctx  context.Context
	tail []byte
	seen bool
}

func (b *timingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.seen {
		data := append(b.tail, p[:n]...)
		if bytes.Contains(data, []byte("\n\n")) || bytes.Contains(data, []byte("\r\n\r\n")) {
			b.seen = true
			observeFirst(b.ctx, "first_event")
		}
		if len(data) > 3 {
			data = data[len(data)-3:]
		}
		b.tail = append(b.tail[:0], data...)
	}
	return n, err
}

func observeRawModel(ctx context.Context, name string, data []byte) {
	t := timingFrom(ctx)
	if t == nil {
		return
	}
	if name != "" && len(name) < 50 {
		safeName := name
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
				sum := sha256.Sum256([]byte(name))
				safeName = "other-" + hex.EncodeToString(sum[:6])
				break
			}
		}
		t.mu.Lock()
		key := timingRoute(ctx) + ":event:" + name
		first := !t.first[key]
		t.first[key] = true
		t.mu.Unlock()
		if first && t.observe != nil {
			t.observe(TimingEvent{Stage: "event_kind", Route: timingRoute(ctx), EventName: safeName, Bytes: len(data), ElapsedMS: timingMS(time.Since(t.start))})
		}
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	if inner, ok := obj["data"].(map[string]any); ok {
		obj = inner
	}
	if normalizeEventName(name) == evTokenUsage {
		observeRawUsage(ctx, obj)
		return
	}
	switch normalizeEventName(name) {
	case "plan_item", "output", "response", "assistant_message", "message", "text":
	default:
		return
	}
	if role, ok := obj["role"].(string); ok && role == "user" {
		return
	}
	for _, key := range []string{"thought", "reasoning", "reasoning_content"} {
		if s, ok := obj[key].(string); ok && s != "" {
			observeFirst(ctx, "upstream_reasoning")
		}
	}
	for _, key := range []string{"response", "content", "text"} {
		if s, ok := obj[key].(string); ok && s != "" {
			observeFirst(ctx, "upstream_content")
		}
	}
}

// Restrict diagnostics to known metering keys and numeric observations. Never
// serialize an arbitrary upstream object or any model text into timing output.
func observeRawUsage(ctx context.Context, obj map[string]any) {
	t := timingFrom(ctx)
	if t == nil || t.observe == nil {
		return
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		obj = usage
	}
	e := TimingEvent{Stage: "raw_usage", Route: timingRoute(ctx), ElapsedMS: timingMS(time.Since(t.start))}
	for _, key := range []string{"prompt_tokens", "input_token", "input_tokens", "completion_tokens", "output_token", "output_tokens", "total_tokens", "cache_read_input_tokens", "cached_tokens", "prompt_cache_hit_tokens", "reasoning_tokens"} {
		if _, present := obj[key]; present {
			e.Fields = append(e.Fields, key)
		}
	}
	if details, ok := obj["prompt_tokens_details"].(map[string]any); ok {
		if _, present := details["cached_tokens"]; present {
			e.Fields = append(e.Fields, "prompt_tokens_details.cached_tokens")
		}
	}
	mapped := mapUsage(obj)
	if n, ok := mapped["prompt_tokens"].(int64); ok {
		e.InputTokens = &n
	}
	if n, ok := mapped["completion_tokens"].(int64); ok {
		e.OutputTokens = &n
	}
	if details, ok := mapped["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := details["cached_tokens"].(int64); ok {
			e.CachedTokens = &n
		}
	}
	t.observe(e)
}
