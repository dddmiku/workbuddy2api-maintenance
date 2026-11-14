// ═══ 更新日志 ═══
// 2026-11-15：默认空账单、商业缓存计数及缺失/非法值回归。
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
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fetchSessionCreditFixture(t *testing.T, payload string) (SessionCredit, bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	t.Cleanup(func() { GatewayBaseVar = old })
	return FetchSessionCredit(context.Background(), testAuth(), "fixture-turn")
}

func TestFetchSessionCreditDefaultBlankBillNotFree(t *testing.T) {
	// 2026-11-15：未知轮次会返回 HTTP 200 和完整默认结构，而非空对象。
	const payload = `{"user_usage_group_by_session":{
		"session_id":"","model_name":"","credits_float":0,"amount_float":0,
		"extra_info":{"input_token":0,"output_token":0,"cache_read_token":0,"cache_write_token":0},
		"usage_group_details":[]}}`
	if _, ok := fetchSessionCreditFixture(t, payload); ok {
		t.Fatal("默认空账单应为未观测，不能当作已观测的免费账单")
	}
}

func TestFetchSessionCreditSessionAssociation(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		wantOK  bool
		matched bool
	}{
		{"精确匹配", `"session_id":"fixture-turn",`, true, true},
		{"错误轮次不能读成当前账单", `"session_id":"another-turn",`, false, false},
		{"未回显仍可兼容扣费", ``, true, false},
		{"空值不能当作已匹配", `"session_id":"",`, true, false},
		{"null不能当作已匹配", `"session_id":null,`, true, false},
		{"错误类型不能关联", `"session_id":123,`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credit, ok := fetchSessionCreditFixture(t, `{"user_usage_group_by_session":{`+tc.field+`"model_name":"test-model","credits_float":0.06,"extra_info":{"input_token":61047,"output_token":2,"cache_read_token":60672}}}`)
			if ok != tc.wantOK || credit.SessionMatched != tc.matched {
				t.Fatalf("ok=%v matched=%v，期望ok=%v matched=%v", ok, credit.SessionMatched, tc.wantOK, tc.matched)
			}
		})
	}
}

func TestFetchSessionCreditCacheAttributionSafety(t *testing.T) {
	const rootExtra = `"extra_info":{"input_token":61047,"output_token":2,"cache_read_token":60672,"cache_write_token":0}`
	cases := []struct {
		name    string
		details string
		want    bool
	}{
		{"缺少明细可按顶层对齐", ``, true},
		{"一组完全一致", `,"usage_group_details":[{"model_display_name":"test-model",` + rootExtra + `}]`, true},
		{"仅提供缓存字段且一致", `,"usage_group_details":[{"extra_info":{"cache_read_token":60672}}]`, true},
		{"未提供对照字段", `,"usage_group_details":[{}]`, true},
		{"显式空分组", `,"usage_group_details":[]`, false},
		{"多分组", `,"usage_group_details":[{},{}]`, false},
		{"null分组", `,"usage_group_details":null`, false},
		{"非数组", `,"usage_group_details":{}`, false},
		{"数组元素非对象", `,"usage_group_details":[null]`, false},
		{"明细extra非对象", `,"usage_group_details":[{"extra_info":null}]`, false},
		{"输入不一致", `,"usage_group_details":[{"extra_info":{"input_token":353080}}]`, false},
		{"输出不一致", `,"usage_group_details":[{"extra_info":{"output_token":225}}]`, false},
		{"缓存读取不一致", `,"usage_group_details":[{"extra_info":{"cache_read_token":316800}}]`, false},
		{"缓存写入不一致", `,"usage_group_details":[{"extra_info":{"cache_write_token":32}}]`, false},
		{"缓存缺效不能与默认零匹配", `,"usage_group_details":[{"extra_info":{"cache_write_token":null}}]`, false},
		{"非法小数", `,"usage_group_details":[{"extra_info":{"input_token":61047.5}}]`, false},
		{"不同模型", `,"usage_group_details":[{"model_display_name":"different-model"}]`, false},
		{"模型类型错误", `,"usage_group_details":[{"model_display_name":null}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"user_usage_group_by_session":{"session_id":"fixture-turn","model_name":"test-model","credits_float":0.06,` + rootExtra + tc.details + `}}`
			credit, ok := fetchSessionCreditFixture(t, payload)
			if !ok || !credit.SessionMatched {
				t.Fatal("分组不可靠只应阻止缓存归因，不应丢弃已匹配账单的真实扣费")
			}
			if credit.CacheAttributionSafe != tc.want {
				t.Errorf("CacheAttributionSafe=%v，期望%v", credit.CacheAttributionSafe, tc.want)
			}
		})
	}
}

func TestFetchSessionCreditCacheAttributionRequiresRootEvidence(t *testing.T) {
	cases := []struct {
		name  string
		group string
	}{
		{"缺少轮次关联", `"model_name":"test-model","extra_info":{"input_token":10,"output_token":1,"cache_read_token":8}`},
		{"明细字段在顶层缺失", `"session_id":"fixture-turn","model_name":"test-model","extra_info":{"input_token":10,"output_token":1},"usage_group_details":[{"extra_info":{"cache_read_token":0}}]`},
		{"两边非法零不能视为一致", `"session_id":"fixture-turn","model_name":"test-model","extra_info":{"cache_read_token":null},"usage_group_details":[{"extra_info":{"cache_read_token":null}}]`},
		{"明细模型没有顶层模型对照", `"session_id":"fixture-turn","extra_info":{"input_token":10,"output_token":1},"usage_group_details":[{"model_display_name":"test-model"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credit, ok := fetchSessionCreditFixture(t, `{"user_usage_group_by_session":{"credits_float":0.06,`+tc.group+`}}`)
			if !ok {
				t.Fatal("该账单仍应保留扣费观测")
			}
			if credit.CacheAttributionSafe {
				t.Error("缺少可核对的顶层证据，不得补缓存")
			}
		})
	}
}

func TestFetchSessionCreditHistoricalCacheCounts(t *testing.T) {
	// 2026-10-16 真实旧账单的数值和结构；身份字段使用测试假值。
	const payload = `{"user_usage_group_by_session":{
		"session_id":"fixture-turn","model_name":"DeepSeek-V4.1-Flash","credits_float":0.08,
		"extra_info":{"input_token":34764,"output_token":46,"cache_read_token":33664,"cache_write_token":0}}}`
	credit, ok := fetchSessionCreditFixture(t, payload)
	if !ok {
		t.Fatal("真实形态的商业账单应可读取")
	}
	if credit.CacheReadTokens != (SessionCacheCount{Value: 33664, Present: true, Valid: true}) {
		t.Errorf("商业账单缓存读取计数丢失：%+v", credit.CacheReadTokens)
	}
	if credit.CacheWriteTokens != (SessionCacheCount{Value: 0, Present: true, Valid: true}) {
		t.Errorf("显式零缓存写入计数丢失：%+v", credit.CacheWriteTokens)
	}
	if !credit.InputTokensPresent || !credit.InputTokensValid || !credit.OutputTokensPresent || !credit.OutputTokensValid {
		t.Error("合法账单输入输出必须标记为已存在且有效，供调用方严格对齐")
	}
}

func TestFetchSessionCreditFreeBillEvidence(t *testing.T) {
	cases := []struct {
		name  string
		group string
		want  bool
	}{
		{"轮次身份", `"session_id":"fixture-turn","model_name":"","extra_info":{"input_token":0}`, true},
		{"模型身份", `"session_id":"","model_name":"test-model"`, true},
		{"实际输入", `"session_id":"","model_name":"","extra_info":{"input_token":1}`, true},
		{"实际缓存读取", `"session_id":"","model_name":"","extra_info":{"cache_read_token":32}`, true},
		{"无有效身份和用量", `"session_id":"  ","model_name":"","extra_info":{"input_token":-1},"usage_group_details":[{}]`, false},
		{"只有账单明细有用量", `"session_id":"","model_name":"","usage_group_details":[{"extra_info":{"input_token":12}}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := fetchSessionCreditFixture(t, `{"user_usage_group_by_session":{"credits_float":0,`+tc.group+`}}`)
			if ok != tc.want {
				t.Fatalf("ok=%v，期望 %v", ok, tc.want)
			}
		})
	}
}

func TestFetchSessionCreditCachePresenceAndValidity(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  SessionCacheCount
	}{
		{"缺失", `{}`, SessionCacheCount{}},
		{"零", `{"cache_read_token":0}`, SessionCacheCount{Present: true, Valid: true}},
		{"正值", `{"cache_read_token":57856}`, SessionCacheCount{Value: 57856, Present: true, Valid: true}},
		{"null", `{"cache_read_token":null}`, SessionCacheCount{Present: true}},
		{"字符串", `{"cache_read_token":"32"}`, SessionCacheCount{Present: true}},
		{"布尔", `{"cache_read_token":true}`, SessionCacheCount{Present: true}},
		{"负数", `{"cache_read_token":-1}`, SessionCacheCount{Present: true}},
		{"小数", `{"cache_read_token":1.5}`, SessionCacheCount{Present: true}},
		{"安全整数边界", `{"cache_read_token":9007199254740991}`, SessionCacheCount{Value: 9007199254740991, Present: true, Valid: true}},
		{"超出安全整数", `{"cache_read_token":9007199254740992}`, SessionCacheCount{Present: true}},
		{"临界小数不可先舍入", `{"cache_read_token":9007199254740990.5}`, SessionCacheCount{Present: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"user_usage_group_by_session":{"model_name":"test-model","credits_float":0,"extra_info":` + tc.extra + `}}`
			credit, ok := fetchSessionCreditFixture(t, payload)
			if !ok {
				t.Fatal("无效可选缓存字段不应丢弃有身份的账单")
			}
			if credit.CacheReadTokens != tc.want {
				t.Errorf("读取观测=%+v，期望=%+v", credit.CacheReadTokens, tc.want)
			}
		})
	}
}

func TestSessionCacheCountRejectsNonFiniteValues(t *testing.T) {
	// JSON 不能表示 NaN/Inf；直接检查共享整数解析约束的防御路径。
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		count := sessionBillCount(map[string]any{"cache_read_token": value}, "cache_read_token")
		if !count.Present || count.Valid {
			t.Error("非有限 token 数须保留存在性，但不能作为有效计数")
		}
	}
}

func TestFetchSessionCreditTokenAlignmentPresence(t *testing.T) {
	cases := []struct {
		name          string
		extra         string
		inputValue    int64
		outputValue   int64
		inputPresent  bool
		inputValid    bool
		outputPresent bool
		outputValid   bool
	}{
		{name: "缺失", extra: `{}`},
		{name: "真实零", extra: `{"input_token":0,"output_token":0}`, inputPresent: true, inputValid: true, outputPresent: true, outputValid: true},
		{name: "实际用量", extra: `{"input_token":61047,"output_token":2}`, inputValue: 61047, outputValue: 2, inputPresent: true, inputValid: true, outputPresent: true, outputValid: true},
		{name: "非法不能用默认零对齐", extra: `{"input_token":1.5,"output_token":"2"}`, inputPresent: true, outputPresent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credit, ok := fetchSessionCreditFixture(t, `{"user_usage_group_by_session":{"model_name":"test-model","credits_float":0,"extra_info":`+tc.extra+`}}`)
			if !ok {
				t.Fatal("带模型身份的零积分账单应保留")
			}
			if credit.InputTokens != tc.inputValue || credit.OutputTokens != tc.outputValue ||
				credit.InputTokensPresent != tc.inputPresent || credit.InputTokensValid != tc.inputValid ||
				credit.OutputTokensPresent != tc.outputPresent || credit.OutputTokensValid != tc.outputValid {
				t.Errorf("输入输出存在性或有效性不正确：%+v", credit)
			}
		})
	}
}

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
		  "session_id":"turn-42",
		  "credits_float":0.42,"extra_info":{"input_token":100,"output_token":200,"cache_read_token":64,"cache_write_token":32},
		  "model_name":"GLM-5.3","use_max_mode":true}}`)
	}))
	defer srv.Close()
	old := GatewayBaseVar
	GatewayBaseVar = srv.URL
	defer func() { GatewayBaseVar = old }()

	// 上游事件流：只有 token_usage（无 credit），与真实形态一致。
	upstream := io.NopCloser(strings.NewReader(
		"event: output\ndata: {\"response\":\"hi\"}\n\n" +
			"event: token_usage\ndata: {\"prompt_tokens\":17,\"completion_tokens\":21,\"total_tokens\":38,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}\n\n" +
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
		details, _ := frame.Usage["prompt_tokens_details"].(map[string]any)
		if details["cached_tokens"] != float64(0) || frame.Usage["cache_creation_input_tokens"] != float64(0) {
			t.Errorf("账单口径不同，不能覆盖单条 SSE 的缓存计数：%+v", frame.Usage)
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
