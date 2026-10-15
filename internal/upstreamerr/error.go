// ═══ 更新日志 ═══
// 2026-10-15：初版——上游错误事件的类型化表示（带业务码）。
//
// 为什么单独一个包：这个类型要**同时**被 upstream 包（通用 SSE 读取器识别它、
// 保住业务码）与 upstream/trae 包（转译层构造它）使用，而 upstream 包已经
// import trae（client.go 的 trae 域出站分支），把它放在任一侧都形成循环依赖。
// 放在叶子包 upstreamerr 里，两边都只依赖它，没有环。
//
// 语义上它确实属于「上游错误」这一层，与具体是哪个上游无关——trae 只是目前
// 唯一会发流内 error 事件的上游。
package upstreamerr

// Error 是上游在**流内**发来的错误事件（SSE 的 event:error），带业务码。
//
// 为什么必须带类型（2026-10-15 修）：trae 转译层原先遇到 error 事件直接
// `fmt.Errorf("%s", message)` 返回普通 error。它经 io.Pipe 传到通用 SSE 读取器后
// 不是任何可识别的类型，被重新包装成 `upstream_read_error`
// （"upstream stream read failed"）——**上游的 code 与原文就此丢失**，网关于是把
// 「确定性的内容/规模拒绝」误判成「上游偶发掐流」，换号重试三次（必然全败，
// 换号改变不了请求体）后报 502。
//
// 实测（2026-10-15）：抓下线上真实失败的出站请求体原样重放，上游稳定回
//
//	{"code":3004,"message":"We're sorry, your requests have exceeded the rate limit..."}
//
// 分界是**请求体字节数**（1621KB 全失败、695KB 及以下全成功），不是调用频次。
type Error struct {
	// Code 上游业务码；0 表示上游没给（只给了文案）。
	Code int
	// Message 上游原文（可能为空）。
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "upstream returned an error event"
}

// RateLimitCode 是上游对「请求超出速率/规模限制」的拒绝码。
//
// 语义要看清：文案是 "exceeded the rate limit"，但实测触发条件是**请求体大小**，
// 不是单位时间内的调用频次——所以正确处置不是「等一会再试」或「换个号」，
// 而是「这个请求太大了」。调用方据此走「不可重试、如实回报」。
const RateLimitCode = 3004

// IsRateLimited 报告该错误是否为限流/规模超限（3004）。
func (e *Error) IsRateLimited() bool { return e.Code == RateLimitCode }
