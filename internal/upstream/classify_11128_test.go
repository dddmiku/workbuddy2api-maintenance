package upstream

import "testing"

// TestClassify11128IsRequestLevelNotAccountBan 11128 是**请求级**拦截，不是账号封禁。
//
// 生产事故（2026-10-16）：面板上 9 个账号被标成
// 「account banned by upstream (11140 request illegal), re-login required」，
// 但 72 小时日志里**一次 11140 都没有**，26 次全是 11128：
//
//	{"code":11128,"msg":"request illegal",
//	 "displayMsg":{"zh":"请求被拦截，请重新发送"}}
//
// 根因：`accountFaultRule` 用关键词 `"request illegal"` 匹配，而 11128 与 11140
// 的**主 msg 逐字相同**（都是 "request illegal"），只有 code 与 displayMsg 不同。
// 只按 msg 判 → 把「这条请求被拦、请重发」误判成「账号被封、需重登」，
// 连续若干次就把健康号停用。
//
// 上游自己写了「请重新发送」——那是**请求级**语义：换一条请求就能过，
// 账号本身没有故障，绝不该计进 NoteAccountFault。
func TestClassify11128IsRequestLevelNotAccountBan(t *testing.T) {
	// 生产原文（2026-10-16 21:37 实测，uid=1e04e34d）。
	body := `{"code":11128,"msg":"request illegal",` +
		`"requestId":"a879cf1ddd28d6621f2fb9893815f379",` +
		`"displayMsg":{"en":"Request blocked. Please send it again",` +
		`"zh":"请求被拦截，请重新发送","zh-hant":"請重新傳送"}}`

	got := Classify(400, body)
	if got == ErrAccountFault {
		t.Fatalf("11128 被误判为账号级故障（会停掉健康号）；上游 displayMsg 写的是「请重新发送」，"+
			"应判请求级可重试类，实际 %v", got)
	}
	// 具体归到哪一类见实现；这里只锁「不是账号故障」这一条硬约束——
	// 归错到 soft_rate 之类仍是可自愈的请求级语义，不会杀号。
}

// TestClassify11140StillAccountFault 真 11140（账号级封禁）必须保持原判定。
//
// 防止为修 11128 而把 11140 一起放宽——那会让真正需要重登的号留在池里反复被选中。
func TestClassify11140StillAccountFault(t *testing.T) {
	cases := []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"error":{"data":{"code":11140,"msg":"request illegal"}}}`,
		`{"code":11140,"msg":"request illegal","displayMsg":{"zh":"账号异常，请重新登录"}}`,
	}
	for _, body := range cases {
		if got := Classify(403, body); got != ErrAccountFault {
			t.Errorf("真 11140 应仍判 ErrAccountFault：body=%s got=%v", body, got)
		}
	}
}
