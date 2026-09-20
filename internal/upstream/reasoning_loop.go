// ═══ 更新日志 ═══
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

const ReasoningLoopErrorCode = "upstream_reasoning_loop"

const (
	reasoningLoopMinChars         = 8000
	reasoningLoopWindow           = 256
	reasoningLoopMaxUnique        = 12
	reasoningLoopCoveragePercent  = 95
	reasoningLoopMaxRepeatedRunes = 32
	reasoningLoopLineBufferRunes  = 4096

	// ReasoningLoopHoldBackBytes 是流式转发在「纯推理、尚无正文/工具进展」阶段压在
	// 内存里的兜底上限，防止分帧很碎的流把缓冲撑爆。
	//
	// 压住不发的意义：命中循环时把这段推理整段丢弃、在同一账号上重发，用户就不会先
	// 看到一段重复文本再看到答案。长推理不能无限等，因此另有字符数上限与「看起来不像
	// 循环就提前放行」的判据。
	ReasoningLoopHoldBackBytes = 2 << 20

	// reasoningLoopHoldBackChars 是压制期的字符上限。线上实测的循环判定点落在
	// 18085–34341 字符之间（自上次实际进展起算），这里取 40K：既覆盖已观测到的全部
	// 形态，又给正常长推理留出可接受的等待上限。到了这个量级还没命中，说明不是这条
	// 规则能抓的循环，继续压只会让长推理迟迟不显示。
	reasoningLoopHoldBackChars = 40 << 10
)

// ReasoningLoopHoldBackTimeout 是压制期的时间上限。字符上限挡不住「想得慢、每次只吐
// 几个字」的流：那种情况下客户端会长时间收不到任何东西。超过这个时长仍未命中就放行，
// 改为实时透传；此后命中只能按原有方式回报错误。
//
// 变量而非常量：测试需要把等待压到毫秒级才能回归这条出口。
var ReasoningLoopHoldBackTimeout = 60 * time.Second

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

type reasoningLineKey struct {
	digest [sha256.Size]byte
	unique uint64
}

type reasoningLoopGuard struct {
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
	if g.characters < reasoningLoopMinChars || g.size != reasoningLoopWindow || len(g.counts) > reasoningLoopMaxUnique {
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
	message := fmt.Sprintf("检测到重复推理循环，已停止该次请求；可整理上下文后重试（本段推理%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
		g.characters, g.size, len(g.counts), repeated, reasoningLoopWindow)
	return &StreamError{Code: ReasoningLoopErrorCode, Message: message, Upstream: map[string]any{
		"code": ReasoningLoopErrorCode, "type": "invalid_request_error", "message": message,
	}}
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
		if choice.loopGuard != nil && choice.loopGuard.characters > maximum {
			maximum = choice.loopGuard.characters
		}
	}
	return maximum
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
	progressed := make(map[int]bool)
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		if state.loopGuard == nil {
			state.loopGuard = &reasoningLoopGuard{}
		}
		if state.loopProgress != state.progress {
			state.loopGuard.reset()
			state.loopProgress = state.progress
			progressed[index] = true
		}
	}
	for _, raw := range choices {
		choice := raw.(map[string]any)
		index := streamChoiceIndex(choice)
		state := s.choices[index]
		if !progressed[index] {
			delta, _ := choice["delta"].(map[string]any)
			text, _ := delta["reasoning_content"].(string)
			if err := state.loopGuard.add(text); err != nil {
				return err
			}
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			if err := state.loopGuard.finish(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamState) finishReasoningLoops() error {
	if s.loopGuardEnabled {
		for _, choice := range s.choices {
			if choice.loopGuard != nil {
				if err := choice.loopGuard.finish(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
