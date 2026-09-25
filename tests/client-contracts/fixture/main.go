// ═══ 更新日志 ═══
// 2026-09-25：官方 SDK 契约测试的独立回环入口；使用真实 Handler、假账号和禁止拨号的确定性上游。
// 2026-09-25：增加双工具及回执续接夹具，保留实际上游请求供测试检查 ID、参数和结果配对。
// 2026-09-25：用可释放的流屏障模拟完整参数后的损坏后缀，避免依赖 sleep 检查提前工具交付。
// 2026-09-25：增加已报告用量后等待取消的流，核对客户端断开确实取消实际转发上下文。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

const apiKey = "fixture-client-contract-key"
const controlKey = "fixture-client-contract-control"
const upstreamOrigin = "http://127.0.0.1:1"

var caseID = regexp.MustCompile(`^[a-z0-9_-]{1,80}$`)

type caseContext struct{}
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type exchange struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	Bytes    int    `json:"bytes"`
	Status   int    `json:"status"`
	Response string `json:"response"`
}

type contractCase struct {
	mu          sync.Mutex
	id          string
	scenario    string
	exchanges   []exchange
	outbound    []json.RawMessage
	finished    int
	canceled    bool
	handler     *server.Handler
	ledger      *usage.Store
	pool        *pool.Pool
	release     chan struct{}
	releaseOnce sync.Once
}

func (c *contractCase) snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{
		"id": c.id, "scenario": c.scenario, "finished_requests": c.finished,
		"upstream_calls": len(c.outbound), "upstream_requests": c.outbound,
		"exchanges": c.exchanges, "upstream_canceled": c.canceled, "usage": c.ledger.Snapshot().Totals,
	}
}

type fixture struct {
	mu       sync.Mutex
	cases    map[string]*contractCase
	dir      string
	mutation string
	stop     chan struct{}
	once     sync.Once
}

func (f *fixture) newCase(id, scenario string) (*contractCase, error) {
	if !caseID.MatchString(id) || (scenario != "text" && scenario != "tools" && scenario != "invalid-tools" && scenario != "cancel") {
		return nil, fmt.Errorf("unsupported fixture case")
	}
	ledger, err := usage.Open(filepath.Join(f.dir, id+".json"), time.Hour)
	if err != nil {
		return nil, err
	}
	c := &contractCase{id: id, scenario: scenario, ledger: ledger, exchanges: []exchange{}, outbound: []json.RawMessage{}, release: make(chan struct{})}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "fixture-account", Domain: "www.workbuddy.ai", AccessToken: "fake-upstream-token", ExpiresAt: 9999999999})
	p.SetCredits("fixture-account", 1000)
	c.pool = p
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme+"://"+r.URL.Host != upstreamOrigin {
			return nil, fmt.Errorf("fixture blocked unexpected upstream origin")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v2/chat/completions" {
			return nil, fmt.Errorf("fixture blocked unexpected upstream operation")
		}
		if r.Context().Value(caseContext{}) != c {
			return nil, fmt.Errorf("fixture request context was lost")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil || !json.Valid(body) {
			return nil, fmt.Errorf("fixture received invalid upstream JSON")
		}
		c.mu.Lock()
		c.outbound = append(c.outbound, append(json.RawMessage(nil), body...))
		c.mu.Unlock()
		reply := textResponse("TEXT_FIXTURE")
		if scenario == "tools" {
			if bytes.Contains(body, []byte("TOOL_RESULT_FIXTURE")) {
				reply = textResponse("TOOL_ROUNDTRIP_OK")
			} else {
				reply = toolsResponse()
			}
		}
		var reader io.ReadCloser = io.NopCloser(strings.NewReader(reply))
		if scenario == "invalid-tools" {
			suffix := frame(map[string]any{"tool_calls": []any{map[string]any{"index": 1, "function": map[string]any{"arguments": "TRAILING_INVALID_SUFFIX"}}}}, nil, nil) + frame(map[string]any{}, "tool_calls", measuredUsage()) + "data: [DONE]\n\n"
			reader = &stagedBody{ctx: r.Context(), c: c, prefix: strings.NewReader(toolPrefix()), suffix: strings.NewReader(suffix), stop: f.stop, closed: make(chan struct{})}
		}
		if scenario == "cancel" {
			prefix := frame(map[string]any{"role": "assistant", "reasoning_content": "THINKING_FIXTURE"}, nil, measuredUsage()) + frame(map[string]any{"content": "TEXT_FIXTUREBEFORE_TERMINAL_FIXTURE"}, nil, nil)
			reader = &stagedBody{ctx: r.Context(), c: c, prefix: strings.NewReader(prefix), suffix: strings.NewReader(""), stop: f.stop, closed: make(chan struct{})}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: r}, nil
	})}
	c.handler = server.NewHandler(server.Config{
		APIKey: apiKey, Pool: p, Usage: ledger, MaxRotate: 1, MaxBodyBytes: 512 << 10, GlobalEnabled: true,
		Upstream: &upstream.Client{HTTP: client, ChatHTTP: client, ChatBaseCN: upstreamOrigin, ChatBaseGlobal: upstreamOrigin, BillingBaseCN: upstreamOrigin, BillingBaseGlobal: upstreamOrigin, GlobalEnabled: true},
	})
	return c, nil
}

func frame(delta map[string]any, finish any, measured map[string]any) string {
	value := map[string]any{
		"id": "chat_contract_fixture", "object": "chat.completion.chunk", "created": 1790294400, "model": "contract-fixture",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if measured != nil {
		value["usage"] = measured
	}
	body, _ := json.Marshal(value)
	return "data: " + string(body) + "\n\n"
}

func measuredUsage() map[string]any {
	return map[string]any{
		"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125, "credit": 0,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 40},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 5},
	}
}

func textResponse(text string) string {
	return frame(map[string]any{"role": "assistant", "reasoning_content": "THINKING_FIXTURE"}, nil, map[string]any{"prompt_tokens": 100}) +
		frame(map[string]any{"content": text}, nil, nil) +
		frame(map[string]any{}, "stop", measuredUsage()) + "data: [DONE]\n\n"
}

func toolPrefix() string {
	calls := []any{}
	for index, city := range []string{"Oslo", "Paris"} {
		arguments, _ := json.Marshal(map[string]string{"city": city})
		calls = append(calls, map[string]any{"index": index, "id": fmt.Sprintf("call_fixture_%d", index), "type": "function", "function": map[string]any{"name": "lookup_weather", "arguments": string(arguments)}})
	}
	return frame(map[string]any{"role": "assistant", "content": "TEXT_FIXTURE", "reasoning_content": "THINKING_FIXTURE"}, nil, nil) +
		frame(map[string]any{"tool_calls": calls}, nil, nil) + frame(map[string]any{"content": "BEFORE_TERMINAL_FIXTURE"}, nil, nil)
}

func toolsResponse() string {
	return toolPrefix() + frame(map[string]any{}, "tool_calls", measuredUsage()) + "data: [DONE]\n\n"
}

// The marker follows complete tool JSON. A client observing it has already had
// a chance to consume any incorrectly exposed tool frames, before the suffix.
type stagedBody struct {
	ctx            context.Context
	c              *contractCase
	prefix, suffix *strings.Reader
	stop, closed   chan struct{}
	once           sync.Once
	released       bool
}

func (b *stagedBody) Read(p []byte) (int, error) {
	if b.prefix.Len() > 0 {
		return b.prefix.Read(p)
	}
	if !b.released {
		select {
		case <-b.c.release:
			b.released = true
		case <-b.ctx.Done():
			b.c.mu.Lock()
			b.c.canceled = true
			b.c.mu.Unlock()
			return 0, b.ctx.Err()
		case <-b.stop:
			return 0, io.EOF
		case <-b.closed:
			return 0, io.EOF
		}
	}
	return b.suffix.Read(p)
}
func (b *stagedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	if b.ctx.Err() != nil {
		b.c.mu.Lock()
		b.c.canceled = true
		b.c.mu.Unlock()
	}
	return nil
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (f *fixture) control(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Contract-Control") != controlKey {
		http.Error(w, "fixture control key required", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/__contract/shutdown" {
		w.WriteHeader(http.StatusNoContent)
		f.once.Do(func() { close(f.stop) })
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/__contract/cases" {
		var spec struct{ ID, Scenario string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&spec) != nil {
			http.Error(w, "invalid fixture case", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, exists := f.cases[spec.ID]; exists {
			http.Error(w, "duplicate fixture case", http.StatusConflict)
			return
		}
		if len(f.cases) >= 128 {
			http.Error(w, "fixture case limit reached", http.StatusTooManyRequests)
			return
		}
		c, err := f.newCase(spec.ID, spec.Scenario)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.cases[spec.ID] = c
		jsonResponse(w, http.StatusCreated, map[string]any{"id": c.id})
		return
	}
	f.mu.Lock()
	c := f.cases[r.URL.Query().Get("id")]
	f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/__contract/release" && c != nil {
		c.releaseOnce.Do(func() { close(c.release) })
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/__contract/case" && c != nil {
		jsonResponse(w, http.StatusOK, c.snapshot())
		return
	}
	http.Error(w, "fixture operation not found", http.StatusNotFound)
}

type captureWriter struct {
	inner    http.ResponseWriter
	status   int
	response bytes.Buffer
	mutation string
}

func (w *captureWriter) Header() http.Header         { return w.inner.Header() }
func (w *captureWriter) Unwrap() http.ResponseWriter { return w.inner }
func (w *captureWriter) WriteHeader(status int) {
	w.status = status
	w.inner.WriteHeader(status)
}
func (w *captureWriter) Write(body []byte) (int, error) {
	original := len(body)
	if w.mutation == "drop-cache" {
		for _, field := range []string{"cached_tokens", "cache_read_input_tokens", "cachedContentTokenCount"} {
			body = bytes.ReplaceAll(body, []byte(`"`+field+`":40`), []byte(`"`+field+`":0`))
		}
	}
	if w.response.Len()+len(body) <= 1<<20 {
		w.response.Write(body)
	}
	_, err := w.inner.Write(body)
	if err != nil {
		return 0, err
	}
	return original, nil
}
func (w *captureWriter) Flush() { _ = w.FlushError() }
func (w *captureWriter) FlushError() error {
	return http.NewResponseController(w.inner).Flush()
}

func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__contract/") {
		f.control(w, r)
		return
	}
	f.mu.Lock()
	c := f.cases[r.Header.Get("X-Client-Contract-ID")]
	f.mu.Unlock()
	if c == nil {
		http.Error(w, "fixture case must be registered", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "fixture input too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), caseContext{}, c))
	encoding := r.Header.Get("Content-Encoding")
	recorder := &captureWriter{inner: w, status: http.StatusOK, mutation: f.mutation}
	c.handler.ServeHTTP(recorder, r)
	c.mu.Lock()
	c.exchanges = append(c.exchanges, exchange{Path: r.URL.Path, Encoding: encoding, Bytes: len(body), Status: recorder.status, Response: recorder.response.String()})
	c.finished++
	c.mu.Unlock()
}

func main() {
	stateDir := flag.String("state-dir", "", "owned temporary directory supplied by the test runner")
	mutation := flag.String("mutation", "", "test-oracle fault: drop-cache")
	flag.Parse()
	if !filepath.IsAbs(*stateDir) || (*mutation != "" && *mutation != "drop-cache") {
		panic("fixture requires an absolute temporary state directory and a known mutation")
	}
	if err := os.MkdirAll(*stateDir, 0700); err != nil {
		panic(err)
	}
	auth.SetGlobalEnabled(true)
	f := &fixture{cases: map[string]*contractCase{}, dir: *stateDir, mutation: *mutation, stop: make(chan struct{})}
	listener := httptest.NewServer(f)
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"fixture": "official-client-contracts", "base_url": listener.URL, "api_key": apiKey})
	<-f.stop
	listener.CloseClientConnections()
	listener.Close()
	for _, c := range f.cases {
		_ = c.ledger.Close()
		c.pool.Close()
	}
}
