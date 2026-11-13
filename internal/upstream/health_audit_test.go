package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestOfficialInputLengthBeforeSystemFlatten(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"system instructions"},{"role":"user","content":"中😀"},{"role":"assistant","content":"ok"},{"role":"user","content":"é"},{"role":"user","content":[{"type":"text","text":"x😀"},{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`)
	out := (&Client{}).prepareBody(body, "cn", "test-account", "test-conversation")
	obj := decodeOfficial(t, out)
	var events []struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal([]byte(obj["extra_vars"].(map[string]any)["growthEvent"].(string)), &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Extra["inputLength"] != float64(9) {
		t.Fatal("growthEvent must count UTF-16 input text before system flattening, excluding images")
	}
	if events[0].Extra["requestModelId"] != obj["model"] {
		t.Fatal("growthEvent model disagrees with outbound model")
	}
	for _, field := range []string{"temperature", "verbosity", "reasoning_summary", "prompt_cache_key", "thinking"} {
		if _, ok := obj[field]; !ok {
			t.Errorf("later transforms lost field %s", field)
		}
	}
}

func TestDiagnosticHeadersHideCredentials(t *testing.T) {
	t.Setenv("WB2API_DUMP_HEADERS", "1")
	sensitive := []string{"authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "x-device-token", "X-Refresh-Token", "X-Api-Key", "X-Custom-Secret"}
	privateValue := strings.Repeat("s", 24)
	identity := "test-account-private-identity"
	headers := map[string][]string{"X-User-Id": {identity}, "x-stainless-package-version": {"6.25.0"}}
	for _, name := range sensitive {
		headers[name] = []string{privateValue}
	}
	var output bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(oldWriter) })
	dumpOutboundHeaders(headers, identity)
	if strings.Contains(output.String(), privateValue) || strings.Contains(output.String(), identity) {
		t.Fatal("header diagnostic exposed private data")
	}
	if !strings.Contains(output.String(), "device-token=present") || !strings.Contains(output.String(), "6.25.0") {
		t.Fatal("redaction lost safe protocol evidence")
	}
	got := headersOf(&http.Request{Header: http.Header(headers)})
	for _, name := range sensitive {
		if !strings.Contains(got[name], "redacted") {
			t.Errorf("credential header %s was not redacted", name)
		}
	}
	if got["X-User-Id"] != logfmtUID(identity) {
		t.Fatal("diagnostic header retained full account identifier")
	}
}

func TestChannelDumpHidesCredentials(t *testing.T) {
	old := channelDumpDir
	SetChannelDumpDir(t.TempDir())
	t.Cleanup(func() { SetChannelDumpDir(old) })
	privateValue := strings.Repeat("s", 24)
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/chat/completions?api_key="+privateValue, nil)
	req.Header.Set("Authorization", privateValue)
	req.Header.Set("X-Device-Token", privateValue)
	(&Client{}).dumpChannelRejection(context.Background(), nil, []byte(`{"messages":[]}`), req, 400, []byte(`{"code":11128}`))
	files, err := filepath.Glob(filepath.Join(channelDumpDir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("channel diagnostic did not produce one record")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(privateValue)) {
		t.Fatal("channel diagnostic retained credentials")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc["request"] != `{"messages":[]}` || doc["response"] != `{"code":11128}` {
		t.Fatal("redaction changed protocol body evidence")
	}
}

type auditCloseBody struct {
	io.Reader
	calls atomic.Int32
	err   error
}

func (b *auditCloseBody) Close() error {
	if b.calls.Add(1) == 1 {
		return b.err
	}
	return nil
}

func TestIdleMonitorConcurrentClosePreservesError(t *testing.T) {
	closeErr := errors.New("fixture close failure")
	rc := &auditCloseBody{Reader: strings.NewReader(""), err: closeErr}
	var canceled atomic.Int32
	body := monitorBody(rc, time.Second, func() { canceled.Add(1) })
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Go(func() { results <- body.Close() })
	}
	wg.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, closeErr) {
			t.Error("concurrent Close lost the initial error")
		}
	}
	if rc.calls.Load() != 1 || canceled.Load() != 1 {
		t.Fatal("monitor performed duplicate resource cleanup")
	}
}

func TestOfficialInputLengthZeroAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		length  int
		present bool
	}{{0, true}, {-1, false}} {
		var events []struct {
			Extra map[string]any `json:"extra"`
		}
		if err := json.Unmarshal([]byte(buildGrowthEvent("test-model", tc.length)), &events); err != nil {
			t.Fatal(err)
		}
		_, present := events[0].Extra["inputLength"]
		if present != tc.present {
			t.Fatal("empty text and unknown input length were conflated")
		}
	}
}

func TestChatDomainRejectsSuffixLookalike(t *testing.T) {
	if chatDomainFor(&auth.Auth{Domain: "otherworkbuddy.cn"}) != "www.workbuddy.cn" {
		t.Fatal("unrelated suffix lookalike was accepted as an official domain")
	}
	if chatDomainFor(&auth.Auth{Domain: "api.workbuddy.cn"}) != "api.workbuddy.cn" {
		t.Fatal("legitimate subdomain compatibility changed")
	}
}

func TestACPRejectsNonHexUUID(t *testing.T) {
	bad := "zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz"
	id := acpConnectionID(bad)
	if uuidFormatted(strings.ReplaceAll(id, "-", "")) == "" {
		t.Fatal("ACP diagnostic shape accepted a non-hex UUID")
	}
}

func BenchmarkOfficialInputLength(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"128KiB", 128 << 10}, {"2MiB", 2 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			obj := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("x", size.bytes)}}}
			b.ReportAllocs()
			b.SetBytes(int64(size.bytes))
			b.ResetTimer()
			for range b.N {
				officialInputLength(obj)
			}
		})
	}
}
