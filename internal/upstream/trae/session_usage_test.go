// ═══ 更新日志 ═══
// 2026-10-18：积分列恒为「—」的修复。
//
// 上游 llm_utils_chat 的 token_usage 事件**只有 token 数、没有 credit 字段**
// （原始事件逐字确认），所以账本永远拿不到扣费。官方客户端另打
// `get_session_usage` 取真实账单，这里在 usage 帧发出前补这一步。
package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMapUsageHasNoCredit 锁住上游事实：token_usage 里没有 credit。
//
// 这条测试的作用不是「验证 bug」，而是**防止有人误以为上游带 credit**：
// 若哪天上游真加了这个字段，mapUsage 应该显式映射它，而不是继续靠查账兜底。
func TestMapUsageHasNoCredit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":     float64(17),
		"completion_tokens": float64(21),
		"total_tokens":      float64(38),
	}
	mapped := mapUsage(usage)
	if _, has := mapped["credit"]; has {
		t.Fatal("上游 token_usage 没有 credit 字段，mapUsage 不该凭空造一个")
	}
	if mapped["prompt_tokens"] != int64(17) || mapped["completion_tokens"] != int64(21) {
		t.Errorf("token 映射错误：%+v", mapped)
	}
}

// TestFetchSessionCredit 解析商业用量接口的响应。
func TestFetchSessionCredit(t *testing.T) {
	const payload = `{"user_usage_group_by_session":{
	  "amount_float":0.07,"cost_money_float":0,"credits_float":0.07,
	  "extra_info":{"cache_read_token":0,"input_token":17,"output_token":84},
	  "model_name":"GLM-5.3","use_max_mode":true,
	  "session_id":"req-abc"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != SessionUsagePath {
			t.Errorf("路径应为 %s，实际 %s", SessionUsagePath, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Cloud-IDE-JWT ") {
			t.Errorf("鉴权头形态错误：%q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["session_id"] != "req-abc" {
			t.Errorf("session_id 应为 req-abc，实际 %q", body["session_id"])
		}
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	defer func() { GatewayBaseVar = old }()

	credit, ok := FetchSessionCredit(context.Background(), testAuth(), "req-abc")
	if !ok {
		t.Fatal("应解析出扣费")
	}
	if credit.Credits != 0.07 {
		t.Errorf("credits 应为 0.07，实际 %v", credit.Credits)
	}
	if credit.InputTokens != 17 || credit.OutputTokens != 84 {
		t.Errorf("token 数解析错误：%+v", credit)
	}
	if credit.ModelName != "GLM-5.3" {
		t.Errorf("模型名应为 GLM-5.3，实际 %q", credit.ModelName)
	}
	if !credit.UseMaxMode {
		t.Error("use_max_mode=true 应被解析出来（确认默认 max 是否真的生效）")
	}
}

// TestFetchSessionCreditMissingIsNotZero 没出账单时 ok=false，绝不返回 0。
//
// 缺失≠0：把「查不到」当成「免费」会让收费账号被误判成免费层，
// 成本账本与选号排序都会跟着错。
func TestFetchSessionCreditMissingIsNotZero(t *testing.T) {
	cases := map[string]string{
		"空对象":     `{}`,
		"分组为空":    `{"user_usage_group_by_session":{}}`,
		"只有非数值字段": `{"user_usage_group_by_session":{"model_name":"GLM-5.3"}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, payload)
			}))
			defer srv.Close()
			old := GatewayBaseVar
			GatewayBaseVar = srv.URL
			defer func() { GatewayBaseVar = old }()

			if credit, ok := FetchSessionCredit(context.Background(), testAuth(), "turn-1"); ok {
				t.Errorf("没出账单时应 ok=false，实际 %+v", credit)
			}
		})
	}
}

// TestFetchSessionCreditZeroIsValid 显式 0 是合法免费观测。
func TestFetchSessionCreditZeroIsValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user_usage_group_by_session":{"credits_float":0,"model_name":"GLM-5.3"}}`)
	}))
	defer srv.Close()
	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	defer func() { GatewayBaseVar = old }()

	credit, ok := FetchSessionCredit(context.Background(), testAuth(), "turn-0")
	if !ok {
		t.Fatal("显式 credits_float=0 是合法观测（免费），不该判为缺失")
	}
	if credit.Credits != 0 {
		t.Errorf("credits 应为 0，实际 %v", credit.Credits)
	}
}

// TestStreamInjectsSessionCredit 流尾把真实扣费写进 usage 帧。
//
// 这是整条链路的终点断言：账本读的是 usage.credit，所以必须出现在发给
// 客户端的 usage 帧里（下游 chatStatsReader 只认这一帧）。
func TestStreamInjectsSessionCredit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user_usage_group_by_session":{
		  "credits_float":0.42,"extra_info":{"input_token":100,"output_token":200},
		  "model_name":"GLM-5.3","use_max_mode":true}}`)
	}))
	defer srv.Close()
	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	defer func() { GatewayBaseVar = old }()

	// 上游事件流：只有 token_usage（无 credit），与真实形态一致。
	upstream := io.NopCloser(strings.NewReader(
		"event: output\ndata: {\"response\":\"hi\"}\n\n" +
			"event: token_usage\ndata: {\"prompt_tokens\":17,\"completion_tokens\":21,\"total_tokens\":38}\n\n" +
			"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))

	stream := translateStreamWithCredit(upstream, "glm-5.3", nil,
		&creditSpec{scope: "test", turnID: "turn-42", fetcher: creditFor(testAuth(), "turn-42")})
	defer stream.Close()

	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	text := string(data)
	// 找到 usage 帧并断言 credit。
	var found bool
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "data: {") || !strings.Contains(line, `"usage"`) {
			continue
		}
		var frame struct {
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame) != nil {
			continue
		}
		if frame.Usage == nil {
			continue
		}
		found = true
		if got, ok := frame.Usage["credit"]; !ok || got != 0.42 {
			t.Errorf("usage.credit 应为 0.42（查账结果），实际 %v（存在=%v）", got, ok)
		}
		// token 数仍来自事件流，不能被查账结果覆盖。
		if frame.Usage["prompt_tokens"] != float64(17) {
			t.Errorf("prompt_tokens 应保持事件流的值 17，实际 %v", frame.Usage["prompt_tokens"])
		}
	}
	if !found {
		t.Fatalf("没找到 usage 帧：\n%s", text)
	}
}

// TestStreamWithoutCreditFetcherOmitsCredit 没有查账回调时不写 credit 字段。
func TestStreamWithoutCreditFetcherOmitsCredit(t *testing.T) {
	upstream := io.NopCloser(strings.NewReader(
		"event: token_usage\ndata: {\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}\n\n" +
			"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"))
	stream := TranslateStreamWithTools(upstream, "glm-5.3", nil)
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if strings.Contains(string(data), `"credit"`) {
		t.Errorf("没有查账回调时不该出现 credit 字段：\n%s", string(data))
	}
}

// TestFetchSessionCreditTimeoutDoesNotHang 查账超时不能拖住整轮。
//
// 查询发生在流尾、[DONE] 之前；若上游卡住，必须由超时兜底而不是无限等。
func TestFetchSessionCreditTimeoutDoesNotHang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	defer func() { GatewayBaseVar = old }()

	start := time.Now()
	_, ok := FetchSessionCredit(context.Background(), testAuth(), "turn-slow")
	if ok {
		t.Error("超时应判为未观测到")
	}
	if elapsed := time.Since(start); elapsed > sessionCreditTimeout+2*time.Second {
		t.Errorf("超时兜底失效，耗时 %v", elapsed)
	}
}

// TestCreditForEmptyTurnID 空 turn id 不发起请求。
func TestCreditForEmptyTurnID(t *testing.T) {
	if creditFor(testAuth(), "") != nil {
		t.Error("空 turn id 不该构造查账回调")
	}
	if creditFor(testAuth(), "   ") != nil {
		t.Error("全空白 turn id 不该构造查账回调")
	}
	if creditFor(nil, "turn-1") != nil {
		t.Error("无账号时不该构造查账回调")
	}
}
