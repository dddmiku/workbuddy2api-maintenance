// ═══ 更新日志 ═══
// 2026-09-20：把重复短行保护扩展到正文（content）。此前只监控 reasoning_content，
// 线上「疯狂输出」实际发生在正文里（窗口 256 行重复覆盖 100%），推理侧达不到阈值，
// 因此既不中断也不重试；现在正文循环单独计数并在命中时中止请求。
// 2026-09-21：退回「命中即停止」单项语义——移除可重发标记、写出闸门与压制缓冲上限；
// 命中就中止该次请求并如实回报错误码，推理与正文恢复实时透传，不再由调用方同账号重发。
// 2026-09-19：为已观察到循环的DeepSeek模型提供有界、可关闭的重复短行推理保护；不改正文、工具或用量。
package upstream

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
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

	// outputLoopMinTopSharePercent 是正文侧独有的额外要求：窗口里出现最多的那一行必须
	// 占多数。正文与推理不同，用户直接看到内容，所以要更保守——否则「两行交替重复」
	// 的表格也会满足「不同项少 + 覆盖率高」而被误截。实测的循环样本是 256 行里 255 行
	// 完全相同（约 99.6%），远高于这条线；两行交替只有 50%。
	outputLoopMinTopSharePercent = 75
)

// StreamOptions supplements the existing Stream/Aggregate APIs. Omitting it
// retains their previous behavior; the HTTP handler explicitly supplies its configuration.
type StreamOptions struct {
	Model              string
	ReasoningLoopGuard bool
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
	nonempty   bool
	pendingCR  bool
	serial     uint64
	window     [reasoningLoopWindow]reasoningLineKey
	position   int
	size       int
	counts     map[reasoningLineKey]int
}

func (g *reasoningLoopGuard) reset() {
	g.characters, g.position, g.size = 0, 0, 0
	g.line = g.line[:0]
	g.overlong, g.nonempty, g.pendingCR = false, false, false
	g.serial = 0
	clear(g.counts)
}

func (g *reasoningLoopGuard) minChars() int {
	if g.output {
		return outputLoopMinChars
	}
	return reasoningLoopMinChars
}

// loopError 生成命中错误。正文侧的文案与错误码都与推理侧区分，调用方据此区分
// 「模型在思考里打转」与「重复正文正在刷屏」。
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
	}
	return state
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
