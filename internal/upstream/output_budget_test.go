// ═══ 更新日志 ═══
// 2026-09-26：锁定输出预算补齐、按上游精确计数收缩、窗口边界 11133 处理与会话主动收缩；历史始终不变。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
)

const (
	budgetModel        = "deepseek-v4.1-flash"
	budgetWindow int64 = 1048576
	// budgetProviderDefault 上游在请求不带 max_tokens 时的默认预留（2026-09-25 线上实测）。
	budgetProviderDefault int64 = 384000
)

// budgetUpstream 模拟上游的窗口判定：输入 + max_tokens（缺省按默认预留）> 窗口即 11115。
type budgetUpstream struct {
	t      *testing.T
	prompt int64
	// reject 可选：返回非空正文时按 400 返回该正文（用于模拟 11133）。
	reject func(maxTokens int64) string

	mu       sync.Mutex
	budgets  []int64 // 每次请求携带的 max_tokens（-1 = 未携带）
	messages [][]any
}

func (u *budgetUpstream) handler(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		u.t.Errorf("read upstream request: %v", err)
	}
	var request struct {
		MaxTokens *json.Number `json:"max_tokens"`
		Messages  []any        `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		u.t.Errorf("invalid upstream request: %v", err)
	}
	budget := int64(-1)
	if request.MaxTokens != nil {
		budget, _ = request.MaxTokens.Int64()
	}
	u.mu.Lock()
	u.budgets = append(u.budgets, budget)
	u.messages = append(u.messages, request.Messages)
	u.mu.Unlock()
	if u.reject != nil {
		if body := u.reject(budget); body != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	reserved := budget
	if reserved < 0 {
		reserved = budgetProviderDefault
	}
	if total := u.prompt + reserved; total > budgetWindow {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"code":11115,"msg":"prompt is too long: %d tokens > %d maximum","extError":{"code":"context_length_exceeded"}}`, total, budgetWindow)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
}

func (u *budgetUpstream) sent() ([]int64, [][]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.budgets...), append([][]any(nil), u.messages...)
}

func budgetClient(t *testing.T, upstream *budgetUpstream, catalog bool) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(upstream.handler))
	t.Cleanup(srv.Close)
	client := &Client{ChatBaseCN: srv.URL, HTTP: srv.Client(), ChatHTTP: srv.Client()}
	if catalog {
		client.storeModelLimits("cn", []ModelInfo{{ID: budgetModel, MaxTokens: 128000, ContextWindow: 1000000}})
	}
	return client
}

func budgetBody(t *testing.T, fields map[string]any, padding int) []byte {
	t.Helper()
	messages := []any{
		map[string]any{"role": "system", "content": "Keep every earlier turn."},
		map[string]any{"role": "user", "content": "first requirement"},
		map[string]any{"role": "assistant", "content": "first answer"},
		map[string]any{"role": "user", "content": strings.Repeat("a ", padding) + "latest question"},
	}
	request := map[string]any{"model": budgetModel, "stream": true, "messages": messages}
	for k, v := range fields {
		request[k] = v
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type budgetOutcome struct {
	status   int
	raw      []byte
	ok       bool
	retries  int
	budgets  []int64
	messages [][]any
}

func runBudget(t *testing.T, client *Client, upstream *budgetUpstream, ctx context.Context, body []byte) budgetOutcome {
	t.Helper()
	var retries int
	var mu sync.Mutex
	ctx = WithChatRetryObserver(ctx, func([]byte) { mu.Lock(); retries++; mu.Unlock() })
	original := bytes.Clone(body)
	rc, status, raw, err := client.ChatStreamContext(ctx, &auth.Auth{UID: "budget-fixture"}, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	ok := rc != nil
	if rc != nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if !bytes.Equal(body, original) {
		t.Fatal("caller body was mutated")
	}
	budgets, messages := upstream.sent()
	return budgetOutcome{status: status, raw: raw, ok: ok, retries: retries, budgets: budgets, messages: messages}
}

func assertHistoryUnchanged(t *testing.T, outcome budgetOutcome) {
	t.Helper()
	for i := 1; i < len(outcome.messages); i++ {
		if !reflect.DeepEqual(outcome.messages[i], outcome.messages[0]) {
			t.Fatalf("attempt %d changed the conversation history", i+1)
		}
	}
}

func TestOutputBudgetFillsCatalogValueOnlyWhenClientOmitsIt(t *testing.T) {
	limits := map[string]modelLimits{budgetModel: {maxOutput: 128000, context: 1000000}}
	cases := []struct {
		name       string
		fields     map[string]any
		limits     map[string]modelLimits
		want       any // 出站 max_tokens（nil = 不携带）
		wantChosen bool
	}{
		{"absent_filled", nil, limits, json.Number("128000"), true},
		{"null_filled", map[string]any{"max_tokens": nil}, limits, json.Number("128000"), true},
		{"explicit_kept", map[string]any{"max_tokens": 4096}, limits, json.Number("4096"), false},
		{"explicit_above_catalog_kept", map[string]any{"max_tokens": 300000}, limits, json.Number("300000"), false},
		{"alias_kept", map[string]any{"max_completion_tokens": 2048}, limits, json.Number("2048"), false},
		{"zero_untouched", map[string]any{"max_tokens": 0}, limits, json.Number("0"), false},
		{"unknown_catalog_absent", nil, nil, nil, false},
		{"other_model_absent", nil, map[string]modelLimits{"other": {maxOutput: 64000}}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, budget := prepareChatBody(budgetBody(t, tc.fields, 1), false, nil, nil, tc.limits)
			var got map[string]any
			decoder := json.NewDecoder(bytes.NewReader(out))
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got["max_tokens"], tc.want) {
				t.Fatalf("max_tokens = %#v, want %#v", got["max_tokens"], tc.want)
			}
			if budget.gatewayChosen != tc.wantChosen {
				t.Fatalf("gatewayChosen = %v, want %v", budget.gatewayChosen, tc.wantChosen)
			}
		})
	}
}

func TestOutputBudgetKeepsLongPromptsInsideWindow(t *testing.T) {
	cases := []struct {
		name        string
		prompt      int64
		catalog     bool
		fields      map[string]any
		wantOK      bool
		wantBudgets []int64
	}{
		// 旧行为：目录未知时不补预算，上游默认预留 384000，664576 以上即超限且不重发。
		{"no_catalog_keeps_single_attempt", 700000, false, nil, false, []int64{-1}},
		// 补齐目录预算后 70 万输入一次成功（修复前这里是 11133/11115）。
		{"filled_budget_700k", 700000, true, nil, true, []int64{128000}},
		{"filled_budget_900k", 900000, true, nil, true, []int64{128000}},
		// 尾段：按上游精确计数收缩到刚好放得下，同号重发一次。
		{"fitted_960k", 960000, true, nil, true, []int64{128000, budgetWindow - 960000 - fitMarginTokens}},
		{"fitted_1m", 1000000, true, nil, true, []int64{128000, budgetWindow - 1000000 - fitMarginTokens}},
		// 剩余空间低于下限：原样返回超限，由客户端压缩。
		{"below_floor_not_retried", 1030000, true, nil, false, []int64{128000}},
		// 客户端显式预算：尊重客户端，不收缩。
		{"explicit_budget_respected", 960000, true, map[string]any{"max_tokens": 128000}, false, []int64{128000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &budgetUpstream{t: t, prompt: tc.prompt}
			client := budgetClient(t, upstream, tc.catalog)
			outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, tc.fields, 1))
			if outcome.ok != tc.wantOK {
				t.Fatalf("success = %v (status %d body %s), want %v", outcome.ok, outcome.status, outcome.raw, tc.wantOK)
			}
			if !reflect.DeepEqual(outcome.budgets, tc.wantBudgets) {
				t.Fatalf("sent budgets %v, want %v", outcome.budgets, tc.wantBudgets)
			}
			if outcome.retries != len(tc.wantBudgets)-1 {
				t.Fatalf("observed %d retries, want %d", outcome.retries, len(tc.wantBudgets)-1)
			}
			if !tc.wantOK && Classify(outcome.status, string(outcome.raw)) != ErrContextTooLong {
				t.Fatalf("final failure is not a context error: %s", outcome.raw)
			}
			assertHistoryUnchanged(t, outcome)
		})
	}
}

const boundary11133 = `{"code":11133,"msg":"Invalid request parameters","requestId":"req-boundary","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider","param":"","type":"invalid_request_error","StatusCode":400}}`

func TestOutputBudgetHandlesBoundaryModelParamRejection(t *testing.T) {
	large := 1300000 // 260 万字符 ≈ 65 万估算 token，加 128000 预算超过目录窗口的 75%
	t.Run("shrunk_budget_succeeds", func(t *testing.T) {
		upstream := &budgetUpstream{t: t, prompt: 0, reject: func(maxTokens int64) string {
			if maxTokens > minFittedOutputTokens {
				return boundary11133
			}
			return ""
		}}
		client := budgetClient(t, upstream, true)
		outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, nil, large))
		if !outcome.ok || !reflect.DeepEqual(outcome.budgets, []int64{128000, minFittedOutputTokens}) {
			t.Fatalf("ok=%v budgets=%v body=%s", outcome.ok, outcome.budgets, outcome.raw)
		}
		assertHistoryUnchanged(t, outcome)
	})
	t.Run("persistent_boundary_becomes_context_error", func(t *testing.T) {
		upstream := &budgetUpstream{t: t, reject: func(int64) string { return boundary11133 }}
		client := budgetClient(t, upstream, true)
		outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, nil, large))
		if outcome.ok || len(outcome.budgets) != 2 {
			t.Fatalf("ok=%v budgets=%v", outcome.ok, outcome.budgets)
		}
		if Classify(outcome.status, string(outcome.raw)) != ErrContextTooLong {
			t.Fatalf("boundary 11133 was not reported as context overflow: %s", outcome.raw)
		}
		detail := ContextTooLongDetail(string(outcome.raw))
		if !strings.Contains(detail, "11133") || !strings.Contains(detail, "req-boundary") {
			t.Fatalf("context detail hides the upstream answer: %s", detail)
		}
		if !strings.Contains(string(outcome.raw), `"upstream"`) {
			t.Fatal("original upstream error was dropped")
		}
	})
	t.Run("explicit_budget_reported_without_retry", func(t *testing.T) {
		upstream := &budgetUpstream{t: t, reject: func(int64) string { return boundary11133 }}
		client := budgetClient(t, upstream, true)
		outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, map[string]any{"max_tokens": 128000}, large))
		if len(outcome.budgets) != 1 || Classify(outcome.status, string(outcome.raw)) != ErrContextTooLong {
			t.Fatalf("budgets=%v body=%s", outcome.budgets, outcome.raw)
		}
	})
	t.Run("small_request_keeps_parameter_error", func(t *testing.T) {
		upstream := &budgetUpstream{t: t, reject: func(int64) string { return boundary11133 }}
		client := budgetClient(t, upstream, true)
		outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, nil, 10))
		if len(outcome.budgets) != 1 || string(outcome.raw) != boundary11133 {
			t.Fatalf("small 11133 changed: budgets=%v body=%s", outcome.budgets, outcome.raw)
		}
	})
	t.Run("named_parameter_keeps_parameter_error", func(t *testing.T) {
		named := strings.Replace(boundary11133, `"param":""`, `"param":"temperature"`, 1)
		upstream := &budgetUpstream{t: t, reject: func(int64) string { return named }}
		client := budgetClient(t, upstream, true)
		outcome := runBudget(t, client, upstream, context.Background(), budgetBody(t, nil, large))
		if len(outcome.budgets) != 1 || string(outcome.raw) != named {
			t.Fatalf("named 11133 changed: budgets=%v body=%s", outcome.budgets, outcome.raw)
		}
	})
}

func TestOutputBudgetProactiveFromSessionUsage(t *testing.T) {
	upstream := &budgetUpstream{t: t, prompt: 960000}
	client := budgetClient(t, upstream, true)
	body := budgetBody(t, nil, 200000)
	// 上一轮：同一会话、同样正文，上游实报 96 万输入；窗口已由超限报文学到。
	client.learnWindow("cn", budgetModel, fmt.Sprintf("prompt is too long: 1100000 tokens > %d maximum", budgetWindow))
	client.RecordPromptSample("session-a", "cn", budgetModel, 960000, body)
	outcome := runBudget(t, client, upstream, WithBudgetSession(context.Background(), "session-a"), body)
	want := budgetWindow - int64(float64(960000)*proactiveSafety) - fitMarginTokens
	if !outcome.ok || !reflect.DeepEqual(outcome.budgets, []int64{want}) {
		t.Fatalf("proactive budget not applied before sending: ok=%v budgets=%v want [%d]", outcome.ok, outcome.budgets, want)
	}
	// 其他会话没有样本：仍按目录预算发送。
	other := &budgetUpstream{t: t, prompt: 100000}
	otherClient := budgetClient(t, other, true)
	otherClient.RecordPromptSample("session-a", "cn", budgetModel, 960000, body)
	outcome = runBudget(t, otherClient, other, WithBudgetSession(context.Background(), "session-b"), body)
	if !reflect.DeepEqual(outcome.budgets, []int64{128000}) {
		t.Fatalf("unrelated session was shrunk: %v", outcome.budgets)
	}
}

func TestRecordPromptSampleDropsShortOrCompactedSessions(t *testing.T) {
	client := &Client{}
	client.storeModelLimits("cn", []ModelInfo{{ID: budgetModel, MaxTokens: 128000, ContextWindow: 1000000}})
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a ", 1000) + `"}]}`)
	client.RecordPromptSample("s", "cn", budgetModel, 900000, body)
	if _, ok := client.samples[sampleKey("s", "cn", budgetModel)]; !ok {
		t.Fatal("long session sample not recorded")
	}
	// 客户端压缩后输入回落：旧样本必须删除，不能再拿压缩前的数字推算。
	client.RecordPromptSample("s", "cn", budgetModel, 120000, body)
	if _, ok := client.samples[sampleKey("s", "cn", budgetModel)]; ok {
		t.Fatal("stale pre-compaction sample kept")
	}
}

func TestEstimatePromptTokensSkipsImageData(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("QUJD", 250000)
	withImage := []byte(`{"content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"` + image + `"}}]}`)
	withoutImage := []byte(`{"content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":""}}]}`)
	if got, want := estimatePromptTokens(withImage), estimatePromptTokens(withoutImage); got != want {
		t.Fatalf("image data counted as text: %d vs %d", got, want)
	}
	if got := estimatePromptTokens([]byte(strings.Repeat("中", 18))); got != 10 {
		t.Fatalf("CJK estimate = %d, want 10", got)
	}
	if got := estimatePromptTokens([]byte(strings.Repeat("a", 40))); got != 10 {
		t.Fatalf("ASCII estimate = %d, want 10", got)
	}
}

func TestFittedOutputBudgetBounds(t *testing.T) {
	chosen := outputBudget{tokens: 128000, gatewayChosen: true}
	detail := func(total int64) string {
		return "prompt is too long: " + strconv.FormatInt(total, 10) + " tokens > 1048576 maximum"
	}
	if got, ok := fittedOutputBudget(chosen, detail(960000+128000)); !ok || got != budgetWindow-960000-fitMarginTokens {
		t.Fatalf("fit = %d %v", got, ok)
	}
	if _, ok := fittedOutputBudget(outputBudget{tokens: 128000}, detail(1088000)); ok {
		t.Fatal("client budget was shrunk")
	}
	if _, ok := fittedOutputBudget(chosen, "prompt is too long"); ok {
		t.Fatal("fit without counts")
	}
	if _, ok := fittedOutputBudget(chosen, detail(1030000+128000)); ok {
		t.Fatal("fit below the output floor")
	}
}
