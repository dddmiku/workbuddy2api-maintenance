// 14003 双形态回归（2026-10-18）。
//
// 背景：生产实测上游 14003 承载两种语义完全不同的答复，主 msg 逐字相同
// （"too many requests"），只有 displayMsg 区分：
//
//	Model busy. Please switch models or retry later   → 模型后端瞬时拥塞
//	Too many requests. Please retry later.            → 来源级限流
//
// 修复前两者都被当成账号限流，各写一条 600s→2h 的模型冷却。用户看到的是
// 「刚注册的号被标成限流」，运维看到的是整片「模型限流 deepseek-v4.1-flash · 解除 22:55」。
// 实测反证：uid=5e3a1b37 22:32:59 撞 Model busy，26 秒后同号同模型即 200 成功。
//
// 新口径：没有精确恢复时刻的一律不算真限流（用户原话）。
// 本文件锁定三件事：
//  1. IsTransientModelBusy 只认 code=14003 + "model busy"，其余 14003 不算；
//  2. 两种形态的 Classify 结果保持 ErrSoftRate（换号语义不变）；
//  3. 无重置时刻时 ParseRateReset 返回 false —— 这是「不写冷却」的判据来源。
package upstream

import (
	"net/http"
	"testing"
)

// busyBody 生产日志原文（requestId 已替换）。中文部分是 GBK 写入日志时损坏的，
// 判定只依赖 en 文案，因此这里保留 en 原样、zh 用占位。
const busyBody = `{"code":14003,"msg":"too many requests","requestId":"197622f3f68174fafbaad01a6291a5a0",` +
	`"displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙，请换模型或稍后重试"}}`

// sourceBody 同 code 的另一种形态：真来源限流。
const sourceBody = `{"code":14003,"msg":"too many requests","requestId":"abc",` +
	`"displayMsg":{"en":"Too many requests. Please retry later.","zh":"请求过于频繁，请稍后重试。"}}`

func TestIsTransientModelBusy(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"model busy", http.StatusTooManyRequests, busyBody, true},
		{"source rate", http.StatusTooManyRequests, sourceBody, false},
		// 状态码不是 429：尚未观测到，不臆造。
		{"busy on 400", http.StatusBadRequest, busyBody, false},
		// code 不对：即便文案里出现 model busy 也不算（防其他业务串味）。
		{"wrong code", http.StatusTooManyRequests,
			`{"code":6004,"msg":"model busy"}`, false},
		// 只有 marker 没有 code：不算。
		{"marker only", http.StatusTooManyRequests, `{"msg":"Model busy"}`, false},
		// 空 body。
		{"empty", http.StatusTooManyRequests, "", false},
		// 大小写不敏感（上游文案首字母大写）。
		{"lowercase marker", http.StatusTooManyRequests,
			`{"code":14003,"displayMsg":{"en":"model busy please switch"}}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransientModelBusy(c.status, c.body); got != c.want {
				t.Fatalf("IsTransientModelBusy(%d, %s)=%v want %v", c.status, c.body, got, c.want)
			}
		})
	}
}

// TestClassify14003BothFormsAreSoftRate 两种形态都必须仍是 ErrSoftRate：
// 「换号重试」的语义不变，变的只是「要不要写冷却」。
func TestClassify14003BothFormsAreSoftRate(t *testing.T) {
	for name, body := range map[string]string{"busy": busyBody, "source": sourceBody} {
		if got := Classify(http.StatusTooManyRequests, body); got != ErrSoftRate {
			t.Errorf("%s: Classify=%v want ErrSoftRate", name, got)
		}
	}
}

// TestParseRateReset14003HasNoResetTime 两种 14003 形态都不带重置墙钟。
// 这正是 handler 侧「不写冷却」的判据——一旦上游开始给时间，这条测试会失败，
// 提醒我们重新评估是否该恢复冷却。
func TestParseRateReset14003HasNoResetTime(t *testing.T) {
	for name, body := range map[string]string{"busy": busyBody, "source": sourceBody} {
		if _, ok := ParseRateReset(body); ok {
			t.Errorf("%s: 14003 不应解析出重置时刻", name)
		}
	}
}
