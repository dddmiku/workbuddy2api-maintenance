// ═══ 更新日志 ═══
// 2026-09-24：新增上下文裁剪的用量回真与超限幅度上限：裁剪过的那次响应必须把
//
//	上游给出的**原始** prompt token 数回传给客户端，否则客户端永远学不到
//	自己的上下文有多大、压缩机制不触发。
//
// context_trim_usage.go 上下文超限的两件事：判据解析与「原始体积」回传。
//
// ── 为什么必须回传原始体积（实测根因） ──
//
// 2026-09-24 实测：客户端真实发出 2015759 token，上游按 1048576 墙拒绝，网关逐级裁剪
// 后返回 200，并把**裁剪后**的 74332 当成 input_tokens 报给客户端。客户端据此认为
// 上下文只有 74k，远低于它自己 900k 的压缩阈值，于是永远不压缩、每轮重发两百万 token，
// 网关每轮再裁三轮——当天 11115 出现 3977 次、自动裁剪 3933 次，全部卡在这个死循环里。
//
// Codex 的压缩由客户端按服务端回传的 token 用量决策（compact_token_budget），
// 参照实现（sub2api 的 native_compaction_v2、new-api 的 Response Compaction）同样
// 依赖如实上报体积。裁剪可以替客户端省一次往返，但绝不能把体积也一起藏起来。
package upstream

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// rePromptTooLong 解析上游 11115 的「prompt is too long: N tokens > M maximum」。
// 同时接受不带 " tokens" 的简化形态（上游文案随版本有微调）。
var rePromptTooLong = regexp.MustCompile(`prompt is too long:\s*(\d+)(?:\s*tokens?)?\s*>\s*(\d+)`)

// PromptTooLongCounts 从 11115 响应体里抽出「本次请求的体积」与「上限」。
// 两者都拿不到时 ok=false；此时调用方不得臆造体积（宁可不裁剪）。
func PromptTooLongCounts(body string) (tokens, maximum int, ok bool) {
	m := rePromptTooLong.FindStringSubmatch(body)
	if len(m) < 3 {
		return 0, 0, false
	}
	tokens, err1 := strconv.Atoi(m[1])
	maximum, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || tokens <= 0 || maximum <= 0 {
		return 0, 0, false
	}
	return tokens, maximum, true
}

// ContextTrimOvershootLimit 是允许自动裁剪的最大超限幅度（相对上游上限）。
//
// 实测需要裁剪的合法场景都是「差一点点」：客户端自己的估算低于上游预检口径，
// 实际只超出几十到几百 token（1048684 / 1048691 / 1048868 对 1048576，约 1.0001）。
// 1.10 给这些场景留足余量。
//
// 超过这条线说明客户端上下文已经远超上限（实测见过 2015759，约 1.92 倍），此时裁剪
// 要丢掉大半对话才能塞进去——用 7% 的历史回答用户，答案本身就已经不可信了。
// 这种情况必须把 11115 原样交回客户端，让它走自己的压缩流程。
const ContextTrimOvershootLimit = 1.10

// WithinTrimOvershootLimit 报告该体积是否值得自动裁剪（而不是把错误交回客户端）。
// tokens/maximum 任一非正时返回 false：没有权威数字就不裁剪。
func WithinTrimOvershootLimit(tokens, maximum int) bool {
	if tokens <= 0 || maximum <= 0 {
		return false
	}
	return float64(tokens) <= float64(maximum)*ContextTrimOvershootLimit
}

// ContextTrimInfo 记录一次请求内上下文裁剪的观测结果。
//
// 由 upstream 客户端在裁剪发生时写入，handler 在把用量交给客户端前读取：
//   - OriginalPromptTokens：上游给出的**裁剪前**真实体积（11115 原文里的 N）。
//     非零时 handler 用它覆盖回传的 input_tokens，让客户端看到真实上下文大小。
//   - Trimmed：本次请求是否发生过裁剪。
type ContextTrimInfo struct {
	OriginalPromptTokens int
	UpstreamMaximum      int
	Trimmed              bool
}

type contextTrimInfoKey struct{}

// WithContextTrimRecorder 在 ctx 上挂一个裁剪观测槽，返回该槽供调用方读取。
// ctx 为 nil 时返回一个孤立槽（不写入 ctx），调用方仍可读，值恒为零值。
func WithContextTrimRecorder(ctx context.Context) (context.Context, *ContextTrimInfo) {
	info := &ContextTrimInfo{}
	if ctx == nil {
		return nil, info
	}
	return context.WithValue(ctx, contextTrimInfoKey{}, info), info
}

// contextTrimInfoFrom 取 ctx 上的裁剪观测槽；未挂时返回 nil。

// streamTrimInfo 从可变参数选项里取裁剪观测槽；未提供时返回 nil（override 零开销）。
func streamTrimInfo(options []StreamOptions) *ContextTrimInfo {
	if len(options) == 0 {
		return nil
	}
	return options[0].TrimInfo
}
func contextTrimInfoFrom(ctx context.Context) *ContextTrimInfo {
	if ctx == nil {
		return nil
	}
	info, _ := ctx.Value(contextTrimInfoKey{}).(*ContextTrimInfo)
	return info
}

// recordContextTrim 记录一次裁剪（首个 11115 的原始体积只记一次，后续档位不覆盖：
// 第二、三档的数字是已经裁过的中间值，比原始值小，覆盖会让客户端低估上下文）。
func recordContextTrim(ctx context.Context, tokens, maximum int) {
	info := contextTrimInfoFrom(ctx)
	if info == nil {
		return
	}
	info.Trimmed = true
	if info.OriginalPromptTokens == 0 && tokens > 0 {
		info.OriginalPromptTokens = tokens
		info.UpstreamMaximum = maximum
	}
}

// overrideUsagePromptSize 把 usage 里的输入体积改写成真实值（保留其它字段原样）。

// override 是 overrideUsagePromptSize 的观测槽方法形态：未裁剪时原样返回，
// 裁剪过时把上游原始体积回真。供 Stream / Aggregate 的用量合并点直接调用。
func (info *ContextTrimInfo) override(usage map[string]any) map[string]any {
	if info == nil || !info.Trimmed {
		return usage
	}
	return overrideUsagePromptSize(usage, info.OriginalPromptTokens)
}

// 上游按裁剪后的请求计费，因此 completion/cache 等字段保持上游原值；只有
// prompt_tokens 与 total_tokens 需要回真——它们是客户端判断上下文大小的唯一依据。
// override<=0 或 usage 为 nil 时原样返回。
func overrideUsagePromptSize(usage map[string]any, override int) map[string]any {
	if usage == nil || override <= 0 {
		return usage
	}
	current := 0
	if v, ok := UsageCount(usage["prompt_tokens"]); ok {
		current = v
	}
	if current >= override {
		return usage // 上游已经报了更大的数：不缩小客户端看到的体积
	}
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	out["prompt_tokens"] = override
	if completion, ok := UsageCount(out["completion_tokens"]); ok {
		out["total_tokens"] = override + completion
	} else if _, present := out["total_tokens"]; present {
		out["total_tokens"] = override
	}
	return out
}

// trimOvershootDescription 供日志与测试描述超限幅度。
func trimOvershootDescription(tokens, maximum int) string {
	if tokens <= 0 || maximum <= 0 {
		return "unknown"
	}
	return strings.TrimRight(strings.TrimRight(
		strconv.FormatFloat(float64(tokens)/float64(maximum), 'f', 3, 64), "0"), ".")
}
