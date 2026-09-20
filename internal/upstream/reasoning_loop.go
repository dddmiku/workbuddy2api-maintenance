// ═══ 更新日志 ═══
// 2026-09-20：把重复短行保护扩展到正文（content）。此前只监控 reasoning_content，
// 线上「疯狂输出」实际发生在正文里（窗口 256 行重复覆盖 100%），推理侧达不到阈值，
// 因此既不中断也不重试；现在正文循环单独计数并在命中时中止请求。
// 2026-09-20：判定命中改为「先截断、再自动重试」——新增 Stream 压制期的缓冲上限，客户端零字节时命中可整段重发（见 sse.go 的写出闸门与 Retryable）。
// 2026-09-19：为已观察到循环的DeepSeek模型提供有界、可关闭的重复短行推理保护；不改正文、工具或用量。
package upstream

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// ReasoningLoopErrorCode 是推理侧（reasoning_content）重复短行的错误码。
	ReasoningLoopErrorCode = "upstream_reasoning_loop"
	// OutputLoopErrorCode 是正文侧（content）重复短行的错误码。与推理侧分开，便于
	// 调用方区分「模型在思考里打转」和「模型已经把重复正文发给用户」。
	OutputLoopErrorCode = "upstream_output_loop"
)

const (
	reasoningLoopMinChars = 8000
	// outputLoopMinChars 是正文侧的字符下限。正文循环（例如整段重复「我执行。」）的
	// 每一行都很短，判定靠的是 256 行窗口而不是字符量。256 行每行至少 1 个非空白
	// 字符加一个分隔符，最少也有 511 个字符，因此这个下限不会挡住满窗口的真实循环，
	// 只是明确表达「正文侧不按字符量判断」。
	outputLoopMinChars            = 256
	reasoningLoopWindow           = 256
	reasoningLoopMaxUnique        = 12
	reasoningLoopCoveragePercent  = 95
	reasoningLoopMaxRepeatedRunes = 32
	reasoningLoopLineBufferRunes  = 4096

	// outputLoopHoldMaxUnique 是正文写出闸门允许压住的「不同短行数」上限。正常回答
	// 只要出现第 3 种不同的短行就被认定为不是循环并立即放行；真正的循环只有一两种
	// 重复行，会一直被压到命中或触发其它上限。
	outputLoopHoldMaxUnique = 2

	// outputLoopHoldBackChars 是正文压制期的字符上限。真实循环在 256 行窗口填满时
	// 就命中，短行循环通常只积累一两千字符，因此这个上限只对「一直是两种短行、却
	// 始终不构成循环」的异常输出生效：到点放行并转为实时透传，避免把正常长输出
	// 一直压在内存里。
	outputLoopHoldBackChars = 32 << 10

	// outputLoopMinTopSharePercent 是正文侧独有的额外要求：窗口里出现最多的那一行必须
	// 占多数。正文与推理不同，用户直接看到内容，所以要更保守——否则「两行交替重复」
	// 的表格也会满足「不同项少 + 覆盖率高」而被误截。实测的循环样本是 256 行里 255 行
	// 完全相同（约 99.6%），远高于这条线；两行交替只有 50%。
	outputLoopMinTopSharePercent = 75

	// ReasoningLoopHoldBackBytes 是流式转发在「纯推理、尚无正文/工具进展」阶段压在
	// 内存里的兜底上限，防止分帧很碎的流把缓冲撑爆。
	//
	// 压住不发的意义：命中循环时把这段推理整段丢弃、在同一账号上重发，用户就不会先
	// 看到一段重复文本再看到答案。长推理不能无限等，因此另有字符数上限与「看起来不像
	// 循环就提前放行」的判据。
	//
	// 这个上限必须显著高于字符上限在线上真实的字节开销，否则它会先于字符上限生效、
	// 把后者变成死条款：上游把推理切成约 220 字节的小帧、每帧只带一两个字符，实测
	// 约 124–222 字节/字符。旧值 2 MiB 折合只有一万多字符，于是缓冲在判定之前就被
	// 放行，客户端先看到一屏重复文本、再收到 422——重发保护形同虚设。16 MiB 覆盖
	// reasoningLoopHoldBackChars（64K 字符 × 222 ≈ 14 MB），让字符与时间上限真正成为
	// 绑定条件。
	//
	// 内存边界：峰值并发实测约 7 条在途请求，按每条最多 16 MiB 计约 112 MB；即便按
	// 24 账号 × 单账号 3 在途全部占满（72 条）也在 1.2 GB 以内，远低于可用内存。
	ReasoningLoopHoldBackBytes = 16 << 20

	// reasoningLoopHoldBackChars 是压制期的字符上限。线上实测的循环判定点落在
	// 9439–55777 字符之间（自上次实际进展起算，判定点是 256 行窗口凑满且重复覆盖达标
	// 的那一帧）。这里取 64K：覆盖已观测到的全部形态，同时给正常长推理留出可接受的
	// 等待上限。到了这个量级还没命中，说明不是这条规则能抓的循环，继续压只会让长推理
	// 迟迟不显示；此时放行并转为实时透传，命中只能照旧报错。
	reasoningLoopHoldBackChars = 64 << 10
)

// ReasoningLoopHoldBackTimeout 是压制期的时间上限。字符上限挡不住「想得慢、每次只吐
// 几个字」的流：那种情况下客户端会长时间收不到任何东西。超过这个时长仍未命中就放行，
// 改为实时透传；此后命中只能按原有方式回报错误。
//
// 变量而非常量：测试需要把等待压到毫秒级才能回归这条出口。
var ReasoningLoopHoldBackTimeout = 60 * time.Second

// ContentLoopHoldBackTimeout 是**正文**压制期的时间上限。正文比推理更该及时显示：
// 一段没有换行的短回答既不可能是短行循环，也不该因为闸门而等满 60 秒。实测的正文
// 循环在几百毫秒内就能填满 256 行窗口，因此这个上限取得比推理侧短得多。
//
// 变量而非常量：测试需要把等待压到毫秒级才能回归这条出口。
var ContentLoopHoldBackTimeout = 8 * time.Second

// StreamOptions supplements the existing Stream/Aggregate APIs. Omitting it
// retains their previous behavior; the HTTP handler explicitly supplies its configuration.
type StreamOptions struct {
	Model              string
	ReasoningLoopGuard bool
	// LoopRetryAvailable 告诉 Stream：命中循环且客户端零字节时，调用方**还会**在同一
	// 账号上重发，因此这次不要向客户端写 error 帧与 [DONE]（避免用户看到半截失败），
	// 只把错误标成 Retryable 返回。
	//
	// 重发次数用尽后调用方必须把它置 false：那时 Stream 按原有方式把错误如实写给客户端。
	// 省略该字段（默认 false）= 原有行为，保持既有调用方兼容。
	LoopRetryAvailable bool
}

func reasoningLoopModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if realm, bare, found := strings.Cut(model, ":"); found {
		switch realm {
		case "cn", "global", "sg":
			model = bare
		default:
			return false
		}
	}
	return model == "deepseek-v4.1-flash"
}

func IsReasoningLoopError(err error) bool {
	var streamErr *StreamError
	return errors.As(err, &streamErr) && streamErr.Code == ReasoningLoopErrorCode
}

// IsLoopGuardError 报告 err 是否由重复短行保护触发（推理侧或正文侧）。
// 调用方按它决定「同账号重发」与失败状态；两类的区别只在错误码与文案。
func IsLoopGuardError(err error) bool {
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		return false
	}
	return streamErr.Code == ReasoningLoopErrorCode || streamErr.Code == OutputLoopErrorCode
}

type reasoningLineKey struct {
	digest [sha256.Size]byte
	unique uint64
}

type reasoningLoopGuard struct {
	// output 区分被监控的文本流：false = reasoning_content，true = content。
	// 只影响字符下限、错误码与文案，检测规则本身一致。
	output     bool
	characters int
	line       []rune
	overlong   bool
	// longLine 记录本段是否出现过超过 reasoningLoopMaxRepeatedRunes 的行。超过上限的
	// 行永远不会被计为重复，因此它的出现就足以证明当前输出不是短行循环。行未结束但
	// 已超过上限时同样置位，避免「整段无换行」的长正文被一直压住。
	longLine bool
	// blankLine 记录本段是否出现过空行（段落分隔）。已观测到的循环是连续的单行重复，
	// 没有空行；正常中文回答与代码块常有空行，因此它的出现足以放行。
	blankLine bool
	nonempty  bool
	pendingCR bool
	serial    uint64
	window    [reasoningLoopWindow]reasoningLineKey
	position  int
	size      int
	counts    map[reasoningLineKey]int
}

func (g *reasoningLoopGuard) reset() {
	g.characters, g.position, g.size = 0, 0, 0
	g.line = g.line[:0]
	g.overlong, g.nonempty, g.pendingCR = false, false, false
	g.longLine = false
	g.blankLine = false
	g.serial = 0
	clear(g.counts)
}

func (g *reasoningLoopGuard) minChars() int {
	if g.output {
		return outputLoopMinChars
	}
	return reasoningLoopMinChars
}

// holdOutput 报告正文是否仍可能是「短行循环」，从而必须先压在内存里不发。
//
// 压住的意义：命中时客户端还是零字节，可以整段丢弃并在同一账号上重发，用户看不到
// 那 256 行重复文本。放行条件（任一成立即说明不是这条规则能抓的循环）：
//   - 出现过超过 32 字符的行——这种行永远不会计为重复；
//   - 不同短行超过 2 个——正常的列表、代码、表格很快就会超过；
//   - 出现过空行（段落分隔）——循环不会产生空行。
//
// 已结束的行数一旦超过窗口长度，窗口就只反映最近 256 行；此前的行已被移出，因此
// 不能再用「已结束行数」判断是否还处于不确定区，必须等出现第三种短行或其它上限。
//
// 正文从第一帧就压住，而不是等到「看起来重复」再压：只有零字节才谈得上整段重发，
// 一旦放出去一行就收不回，循环就会漏到用户眼前。代价是首字延迟，由几条上限兜底：
// 出现长行或空行立即放行、累计到 outputLoopHoldBackChars 放行、以及 sse.go 里共用的
// 60 秒与 2 MiB 兜底。正常回答的第一行通常就超过 32 字符，因此实际只多等一个行尾。
func (g *reasoningLoopGuard) holdOutput() bool {
	// 本段还没有正文：没有东西需要压住。refusal 或工具进展后的空窗期也走这里，
	// 让那些帧照常实时到达客户端。
	if g.characters == 0 {
		return false
	}
	if g.longLine || g.blankLine || len(g.counts) > outputLoopHoldMaxUnique ||
		g.characters >= outputLoopHoldBackChars {
		return false
	}
	return true
}

// loopError 生成命中错误。正文侧的文案与错误码都与推理侧区分：正文已经可能发给
// 客户端，调用方需要据此判断「能不能整段重发」。
func (g *reasoningLoopGuard) loopError(repeated int) *StreamError {
	code := ReasoningLoopErrorCode
	var message string
	if g.output {
		code = OutputLoopErrorCode
		message = fmt.Sprintf("检测到重复输出循环，已停止该次请求；可整理上下文后重试（本段正文%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
			g.characters, g.size, len(g.counts), repeated, reasoningLoopWindow)
	} else {
		message = fmt.Sprintf("检测到重复推理循环，已停止该次请求；可整理上下文后重试（本段推理%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
			g.characters, g.size, len(g.counts), repeated, reasoningLoopWindow)
	}
	return &StreamError{Code: code, Message: message, Upstream: map[string]any{
		"code": code, "type": "invalid_request_error", "message": message,
	}}
}

func reasoningSpace(r rune) bool {
	// Python str.strip also recognizes these ASCII separators.
	return unicode.IsSpace(r) || (r >= '\x1c' && r <= '\x1f')
}

func reasoningLineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

func (g *reasoningLoopGuard) add(text string) error {
	for _, r := range text {
		if g.pendingCR {
			g.pendingCR = false
			if r == '\n' {
				g.countCharacter()
				if err := g.completeLine(); err != nil {
					return err
				}
				continue
			}
			if err := g.completeLine(); err != nil {
				return err
			}
		}
		g.countCharacter()
		if r == '\r' {
			g.pendingCR = true
			continue
		}
		if reasoningLineBreak(r) {
			if err := g.completeLine(); err != nil {
				return err
			}
			continue
		}
		g.nonempty = g.nonempty || !reasoningSpace(r)
		if !g.overlong {
			if g.line == nil {
				g.line = make([]rune, 0, reasoningLoopLineBufferRunes)
			}
			if len(g.line) == reasoningLoopLineBufferRunes {
				g.overlong = true
				g.line = g.line[:0]
			} else {
				g.line = append(g.line, r)
				// 行还没结束，但只要已经超过重复上限，这行就注定不可能计为重复：
				// 立即认定为「不是短行循环」，长段落不会因为等不到换行而被压住。
				if len(g.line) > reasoningLoopMaxRepeatedRunes {
					g.longLine = true
				}
			}
		}
	}
	return nil
}

func (g *reasoningLoopGuard) countCharacter() {
	if g.characters < int(^uint(0)>>1) {
		g.characters++
	}
}

func (g *reasoningLoopGuard) finish() error {
	g.pendingCR = false
	return g.completeLine()
}

func (g *reasoningLoopGuard) completeLine() error {
	if !g.nonempty {
		g.line = g.line[:0]
		g.overlong = false
		g.blankLine = true
		return nil
	}
	key := reasoningLineKey{}
	text := ""
	if !g.overlong {
		text = strings.TrimFunc(string(g.line), reasoningSpace)
	}
	if g.overlong || utf8.RuneCountInString(text) > reasoningLoopMaxRepeatedRunes {
		// Long lines still occupy the window, but never increase repetition.
		// Neither their text nor their unbounded length is retained in the detector.
		g.serial++
		key.unique = g.serial
		g.longLine = true
	} else {
		key.digest = sha256.Sum256([]byte(text))
	}
	g.line = g.line[:0]
	g.overlong, g.nonempty = false, false
	if g.counts == nil {
		g.counts = make(map[reasoningLineKey]int, reasoningLoopWindow)
	}
	if g.size == reasoningLoopWindow {
		old := g.window[g.position]
		g.counts[old]--
		if g.counts[old] == 0 {
			delete(g.counts, old)
		}
	} else {
		g.size++
	}
	g.window[g.position] = key
	g.position = (g.position + 1) % reasoningLoopWindow
	g.counts[key]++
	if g.characters < g.minChars() || g.size != reasoningLoopWindow || len(g.counts) > reasoningLoopMaxUnique {
		return nil
	}
	repeated := 0
	for _, count := range g.counts {
		if count >= 2 {
			repeated += count
		}
	}
	if repeated*100 < reasoningLoopCoveragePercent*reasoningLoopWindow {
		return nil
	}
	// 正文侧额外要求一个占主导的重复行：三行交替重复的表格不该被当成循环。
	if g.output {
		top := 0
		for _, count := range g.counts {
			if count > top {
				top = count
			}
		}
		if top*100 < outputLoopMinTopSharePercent*reasoningLoopWindow {
			return nil
		}
	}
	return g.loopError(repeated)
}

func newStreamState(options []StreamOptions) *streamState {
	state := &streamState{}
	if len(options) > 0 {
		state.loopGuardEnabled = options[0].ReasoningLoopGuard && reasoningLoopModel(options[0].Model)
		state.loopRetryAvailable = options[0].LoopRetryAvailable
	}
	return state
}

// reasoningCharacters 返回各 choice 已累计的推理字符数最大值，供写出闸门判断
// 「重复推理判定该触发就触发过了」，从而结束压制、恢复实时透传。
func (s *streamState) reasoningCharacters() int {
	maximum := 0
	for _, choice := range s.choices {
		if choice.reasoningGuard != nil && choice.reasoningGuard.characters > maximum {
			maximum = choice.reasoningGuard.characters
		}
	}
	return maximum
}

// holdOutput 汇总各 choice 的正文闸门判据：只要还有一个 choice 没能排除「短行循环」，
// 就继续把正文压在内存里。多 choice 场景下按最不确定的那个判断，避免漏掉循环分支。
func (s *streamState) holdOutput() bool {
	for _, choice := range s.choices {
		guard := choice.outputGuard
		if guard != nil && guard.holdOutput() {
			return true
		}
	}
	return false
}

func streamChoiceIndex(choice map[string]any) int {
	index, _ := choice["index"].(float64)
	return int(index)
}

func (s *streamState) observeReasoningLoops(obj map[string]any) error {
	if !s.loopGuardEnabled {
		return nil
	}
	choices, _ := obj["choices"].([]any)
	reasoningReset := make(map[int]bool, len(choices))
	outputReset := make(map[int]bool, len(choices))
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		if state.reasoningGuard == nil {
			state.reasoningGuard = &reasoningLoopGuard{}
			state.outputGuard = &reasoningLoopGuard{output: true}
		}
		// 推理窗口由任意可见进展重置（原有行为）。
		if state.reasoningGuardProgress != state.progress {
			state.reasoningGuard.reset()
			state.reasoningGuardProgress = state.progress
			reasoningReset[index] = true
		}
		// 正文窗口只由非正文进展重置：正文本身正是被监控的载荷，不能自我重置。
		if state.outputGuardProgress != state.nonContentProgress {
			state.outputGuard.reset()
			state.outputGuardProgress = state.nonContentProgress
			outputReset[index] = true
		}
	}
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		delta, _ := choice["delta"].(map[string]any)
		if !reasoningReset[index] {
			text, _ := delta["reasoning_content"].(string)
			if err := state.reasoningGuard.add(text); err != nil {
				return err
			}
		}
		if !outputReset[index] {
			text, _ := delta["content"].(string)
			if err := state.outputGuard.add(text); err != nil {
				return err
			}
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			if err := state.reasoningGuard.finish(); err != nil {
				return err
			}
			if err := state.outputGuard.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) finishReasoningLoops() error {
	if s.loopGuardEnabled {
		for _, choice := range s.choices {
			if choice.reasoningGuard != nil {
				if err := choice.reasoningGuard.finish(); err != nil {
					return err
				}
			}
			if choice.outputGuard != nil {
				if err := choice.outputGuard.finish(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
