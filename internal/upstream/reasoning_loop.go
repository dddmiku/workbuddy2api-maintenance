// ═══ 更新日志 ═══
// 2026-09-22：修掉「明明在抽风却检测不到」的根因，并取消按推理字符量判定。
// 旧实现把每一条非空行都算成一个窗口格位，可长行（>32 字符）永远不可能重复，
// 于是覆盖率被结构性压住：实测两份真实循环里长行分别占 19.2% 与 42.9% 的格位，
// 覆盖率天花板被压到约 81% 与 57%，95% 这条线只能等整段输出快结束才勉强达标。
// 现在长行不再占窗口格位，窗口只统计「可重复的短行」，判定纯看重复形态；
// 窗口同时从 256 行缩到 32 行，命中点从整段的 99% 提前到重复真正开始的时刻。
// 推理侧的字符下限（8000→5500）一并删除：它只会再造一次「参数挡住检测」的问题。
// 同时新增代码块防线：``` 围栏内的行（以及围栏标记本身）不进入窗口。实测真实
// 推理里的代码片段会反复出现 `}`、`status = {`、`...` 这类行，窗口缩短后足以
// 骗过覆盖率判定；排除围栏后本地 4967 段真实推理零误报，两段真实循环仍命中。
// 2026-09-21：放宽推理侧的漏检阈值。用户两次反馈「模型明显在抽风，但保护没反应」，
// 拿到的两份真实推理文本分别只有 5993 与 6663 字符、窗口内 12–13 种短行，正好卡在旧的
// 8000 字符 / 12 种之外。推理侧下限改为 5500 字符、不同短行上限改为 16；正文侧保持
// 放宽前的 12 并继续要求一行占多数，避免误截表格类正文。放宽只作用于推理侧是因为
// 推理误报的代价只是多一次同账号重发，正文误报会直接截断用户可见内容。
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
	// reasoningLoopWindow / outputLoopWindow 是滑动窗口的长度，单位是「可重复的短行」。
	//
	// 长行（超过 reasoningLoopMaxRepeatedRunes 的行）不再占窗口格位：它们永远不可能
	// 与任何东西重复，留在窗口里只会稀释覆盖率。线上两份真实循环里长行分别占 19.2%
	// 与 42.9% 的非空行，旧语义下覆盖率天花板被压到约 81% 与 57%，95% 这条线要等整段
	// 输出快结束才勉强达标——这就是「明明在抽风，保护却没反应」的根因。
	//
	// 窗口取 32 而不是 256：判定只需要看「最近这一小段是不是在重复」，窗口越短，
	// 命中越早（实测从整段的 99% 提前到重复真正开始的时刻），内存与耗时也更低。
	// 32 已经远大于任何正常写作里连续出现同一句短行的长度。
	reasoningLoopWindow = 32
	// outputLoopWindow 是正文侧的窗口长度。正文直接面向用户，误截代价高，
	// 因此比推理侧更长（64 行）以换取更保守的判定。
	outputLoopWindow = 64
	// reasoningLoopMaxUnique 是窗口内允许的「不同短行数」上限。真实叙述循环用的
	// 是十来句口头禅轮换（实测窗口内 12–13 种）；16 覆盖已观测形态，同时远低于
	// 正常推理的不同行数（实测正常样本都在 200+，几乎不可能撞上）。
	reasoningLoopMaxUnique = 16
	// outputLoopMaxUnique 是正文侧的不同短行上限。正文直接面向用户，比推理侧保守，
	// 沿用放宽前的 12：正文侧另有「一行必须占多数」的要求，两个条件一起把表格、
	// 状态行这类合法重复挡在外面。
	outputLoopMaxUnique = 12

	reasoningLoopCoveragePercent  = 95
	reasoningLoopMaxRepeatedRunes = 32
	reasoningLoopLineBufferRunes  = 4096

	// reasoningLoopMaxFencedLines 是代码围栏的兜底上限。围栏内的行不进入检测窗口，
	// 但上游如果输出了一个始终不闭合的 ```，检测就会被无限期关掉。超过这个行数后
	// 强制退出围栏状态，把后续短行重新纳入检测；正常代码块远达不到这个量级。
	reasoningLoopMaxFencedLines = 512

	// outputLoopHoldMaxUnique 是正文写出闸门允许压住的「不同短行数」上限。正常回答
	// 只要出现第 3 种不同的短行就被认定为不是循环并立即放行；真正的循环只有一两种
	// 重复行，会一直被压到命中或触发其它上限。
	//
	// 三行极短行交替（同属循环形态）另有一条放宽：见 holdOutput 的 tiny 判据——
	// 只有当窗口里每一种行都短到 loopCycleMaxRunes 以内时才多压一种，避免让正常的
	// 三行表格白白多等一个正文压制期。
	outputLoopHoldMaxUnique = 2

	// loopCycleMaxRunes 与 loopCycleMaxLength 是「极短行严格周期循环」的判据上限：
	// 窗口内每种短行都不超过 6 个字符，且不同短行数在 2–3 之间。
	//
	// 存在的理由：主导性兜底挡住了两行/三行交替，而线上确实出现了「好。」(2 字) 与
	// 「执行。」(3 字) 的严格交替。这类输出信息量趋近于零，不可能是有意义的正文；
	// 真正的表格/状态行（如 `| a | b |`、`| --- | --- |`）行长都在 9 字以上，天然被
	// 行长上限挡在外面，因此这条判据与「保护合法重复」并不冲突。
	loopCycleMaxRunes  = 6
	loopCycleMaxLength = 3

	// loopCycleMinRepeats 是周期至少重复多少轮才判定为循环。64 行窗口配 2 行周期即
	// 32 轮，这里取 8 轮：远高于任何正常写作里连续出现的同句数量，又能让判定在窗口
	// 刚填满时就落地，不必等到正文闸门的字符上限。
	loopCycleMinRepeats = 8

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

	// reasoningLoopMinUniqueWithoutDominance 与 reasoningLoopMinTopSharePercent 是推理侧
	// 的兜底条件：只有当窗口里的不同短行少于 4 种时，才要求出现最多的那一行占多数。
	//
	// 放宽字符下限与不同项上限之后，两行/三行交替的表格、状态行这类「合法重复」会满足
	// 「不同项少 + 覆盖率高」。它们没有一行占主导（实测两行交替 50%、三行交替约 34%），
	// 用主导性即可挡住。反过来，线上观测到的叙述循环用的都是十来句口头禅轮换
	// （窗口内 11–13 种，最多的一种只占 25%–33%），不同项一旦达到 4 种就不再要求主导性，
	// 因此不会被这条兜底挡在门外。单行重复（唯一行 1、占比 100%）也照常命中。
	reasoningLoopMinUniqueWithoutDominance = 4
	reasoningLoopMinTopSharePercent        = 75

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
}

type reasoningLoopGuard struct {
	// output 区分被监控的文本流：false = reasoning_content，true = content。
	// 只影响窗口长度、错误码与文案，检测规则本身一致。
	output     bool
	characters int
	line       []rune
	overlong   bool
	// longLine 记录本段是否出现过超过 reasoningLoopMaxRepeatedRunes 的行。这类行永远
	// 不会被计为重复，因此不占窗口格位；它们只用于正文写出闸门的放行判断。
	longLine bool
	// blankLine 记录本段是否出现过空行（段落分隔）。已观测到的循环是连续的单行重复，
	// 没有空行；正常中文回答与代码块常有空行，因此它的出现足以放行正文闸门。
	blankLine bool
	nonempty  bool
	pendingCR bool
	// fenced 记录当前行是否位于 ``` 代码围栏内部。围栏内的行（以及围栏标记本身）
	// 不进入窗口：真实推理里的代码片段会反复出现 `}`、`status = {`、`...` 这类行，
	// 它们天然重复，但属于合法输出，不是循环。
	fenced bool
	// fencedLines 记录当前围栏已经跳过了多少行，用于兜住始终不闭合的围栏。
	fencedLines int
	// window 只保存「可重复的短行」。长度由 windowSize() 决定，按流类型取
	// reasoningLoopWindow 或 outputLoopWindow。
	window   []reasoningLineKey
	position int
	size     int
	counts   map[reasoningLineKey]int
	// lengths 记录窗口内每种短行的字符数，供「极短行严格周期循环」判定使用。
	// 与 counts 同步增删。
	lengths map[reasoningLineKey]int
}

func (g *reasoningLoopGuard) reset() {
	g.characters, g.position, g.size = 0, 0, 0
	g.line = g.line[:0]
	g.overlong, g.nonempty, g.pendingCR = false, false, false
	g.longLine = false
	g.blankLine = false
	g.fenced = false
	g.fencedLines = 0
	g.window = g.window[:0]
	clear(g.counts)
	clear(g.lengths)
}

// windowSize 返回该流使用的窗口长度（单位：可重复的短行）。
func (g *reasoningLoopGuard) windowSize() int {
	if g.output {
		return outputLoopWindow
	}
	return reasoningLoopWindow
}

// maxUnique 是窗口内允许的不同短行数上限。推理侧放宽到 16 以覆盖真实叙述循环，
// 正文侧保持放宽前的 12：正文直接面向用户，误截代价更高。
func (g *reasoningLoopGuard) maxUnique() int {
	if g.output {
		return outputLoopMaxUnique
	}
	return reasoningLoopMaxUnique
}

// holdOutput 报告正文是否仍可能是「短行循环」，从而必须先压在内存里不发。
//
// 压住的意义：命中时客户端还是零字节，可以整段丢弃并在同一账号上重发，用户看不到
// 那几十行重复文本。放行条件（任一成立即说明不是这条规则能抓的循环）：
//   - 出现过超过 32 字符的行——这种行永远不会计为重复；
//   - 不同短行超过 2 个——正常的列表、代码、表格很快就会超过；
//   - 出现过空行（段落分隔）——循环不会产生空行。
//
// 已结束的行数一旦超过窗口长度，窗口就只反映最近 64 条短行；此前的行已被移出，因此
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
	if g.longLine || g.blankLine || len(g.counts) > g.holdUniqueLimit() ||
		g.characters >= outputLoopHoldBackChars {
		return false
	}
	return true
}

// holdUniqueLimit 是正文闸门允许压住的「不同短行数」上限。默认 outputLoopHoldMaxUnique；
// 当窗口里每一种行都短到 loopCycleMaxRunes 以内时放宽到 loopCycleMaxLength，让三行极短行
// 交替也能被整段重发。表格行普遍更长，因此不会被这条放宽牵连。
func (g *reasoningLoopGuard) holdUniqueLimit() int {
	if len(g.counts) == 0 || len(g.counts) > loopCycleMaxLength {
		return outputLoopHoldMaxUnique
	}
	for key := range g.counts {
		if length, ok := g.lengths[key]; !ok || length > loopCycleMaxRunes {
			return outputLoopHoldMaxUnique
		}
	}
	return loopCycleMaxLength
}

// loopError 生成命中错误。正文侧的文案与错误码都与推理侧区分：正文已经可能发给
// 客户端，调用方需要据此判断「能不能整段重发」。
// isTinyStrictCycle 判定窗口是否为「极短行严格周期循环」：窗口由 2–3 种极短行组成，
// 且整段输出是同一段周期原样重复，而不是随机混排。
//
// 这条判据补的是主导性兜底的缺口：两行交替时各占 50%、三行交替约 34%，都过不了
// 「一行必须占 75%」这条线，于是保护完全不触发（线上「好。」/「执行。」即此形态）。
//
// 两道限定让它与「合法重复」分开：
//   - 行长上限 loopCycleMaxRunes：表格行（`| a | b |`、`| --- | --- |`）与状态行
//     普遍在 9 字以上，直接出局；
//   - 严格周期：窗口必须能整除成同一段周期反复，随机混排的两种短行不算。
func (g *reasoningLoopGuard) isTinyStrictCycle() bool {
	distinct := len(g.counts)
	if distinct < 2 || distinct > loopCycleMaxLength {
		return false
	}
	if g.size < loopCycleMinRepeats*distinct {
		return false
	}
	for key := range g.counts {
		if length, ok := g.lengths[key]; !ok || length > loopCycleMaxRunes {
			return false
		}
	}
	// 环形窗口里最旧的一行在 g.position（写满后它就是要被覆盖的槽位）。
	ordered := make([]reasoningLineKey, 0, g.size)
	for offset := 0; offset < g.size; offset++ {
		ordered = append(ordered, g.window[(g.position+offset)%g.size])
	}
	// 周期性判定用不变式 s[i] == s[i+period]，对环形窗口的起始相位不敏感；
	// 不能用「窗口长度整除周期」那种写法——窗口 32 遇上三行周期会永远判不出来。
	for period := 1; period <= distinct; period++ {
		cyclic := true
		for index := 0; index+period < g.size; index++ {
			if ordered[index] != ordered[index+period] {
				cyclic = false
				break
			}
		}
		if cyclic {
			return true
		}
	}
	return false
}

func (g *reasoningLoopGuard) loopError(repeated, window int) *StreamError {
	code := ReasoningLoopErrorCode
	var message string
	if g.output {
		code = OutputLoopErrorCode
		message = fmt.Sprintf("检测到重复输出循环，已停止该次请求；可整理上下文后重试（本段正文%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
			g.characters, g.size, len(g.counts), repeated, window)
	} else {
		message = fmt.Sprintf("检测到重复推理循环，已停止该次请求；可整理上下文后重试（本段推理%d字符，窗口%d行，唯一行%d，重复覆盖%d/%d）。",
			g.characters, g.size, len(g.counts), repeated, window)
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
	// 超过行缓冲上限的行文本已不再保留，必须在清标记前记住，否则所有超长行都会
	// 塌缩成空串、互相「重复」，反而制造出假循环。
	overlong := g.overlong
	if !overlong {
		text = strings.TrimFunc(string(g.line), reasoningSpace)
	}
	g.line = g.line[:0]
	g.overlong, g.nonempty = false, false

	if overlong {
		g.longLine = true
		return nil
	}

	// 代码围栏：标记行本身与围栏内部的行都不进入窗口。围栏内的代码天然带重复
	// 结构（`}`、`...`、`status = {`），把它们算进覆盖率会误伤正常推理。
	if strings.HasPrefix(text, "```") {
		g.fenced = !g.fenced
		g.fencedLines = 0
		return nil
	}
	if g.fenced {
		// 兜底：始终不闭合的围栏不能把检测永久关掉。
		g.fencedLines++
		if g.fencedLines > reasoningLoopMaxFencedLines {
			g.fenced = false
			g.fencedLines = 0
		}
		return nil
	}

	// 长行（含超出行缓冲上限的行）永远不可能与任何东西重复，因此不进入窗口：
	// 让它们占格位只会稀释覆盖率，把真正的循环挡在判定线之外。它们只用于正文
	// 写出闸门的「看起来不像循环就放行」判断。
	if utf8.RuneCountInString(text) > reasoningLoopMaxRepeatedRunes {
		g.longLine = true
		return nil
	}
	key.digest = sha256.Sum256([]byte(text))

	if g.counts == nil {
		g.counts = make(map[reasoningLineKey]int, g.windowSize())
	}
	if g.lengths == nil {
		g.lengths = make(map[reasoningLineKey]int, g.windowSize())
	}
	size := g.windowSize()
	if g.size == size {
		old := g.window[g.position]
		g.counts[old]--
		if g.counts[old] == 0 {
			delete(g.counts, old)
			delete(g.lengths, old)
		}
	} else {
		g.size++
	}
	if g.size > len(g.window) {
		g.window = append(g.window, key)
	} else {
		g.window[g.position] = key
	}
	g.position = (g.position + 1) % size
	g.counts[key]++
	g.lengths[key] = utf8.RuneCountInString(text)
	if g.size != size || len(g.counts) > g.maxUnique() {
		return nil
	}
	repeated := 0
	for _, count := range g.counts {
		if count >= 2 {
			repeated += count
		}
	}
	if repeated*100 < reasoningLoopCoveragePercent*size {
		return nil
	}
	// 极短行严格周期循环：两行/三行交替，每行都很短、每轮完全一致。这类输出没有
	// 信息量，必须命中；否则会被下面的主导性兜底挡住（两行各占 50%）。
	if g.isTinyStrictCycle() {
		return g.loopError(repeated, size)
	}
	// 推理侧：不同短行很少时（两行、三行交替）额外要求一行占主导，避免把表格、状态行
	// 这类合法重复当成循环。达到 reasoningLoopMinUniqueWithoutDominance 种之后不再要求，
	// 否则线上那种「十来句口头禅轮换」的真实循环会被挡住。正文侧下面另有同等要求。
	if !g.output && len(g.counts) < reasoningLoopMinUniqueWithoutDominance {
		top := 0
		for _, count := range g.counts {
			if count > top {
				top = count
			}
		}
		if top*100 < reasoningLoopMinTopSharePercent*size {
			return nil
		}
	}
	// 正文侧额外要求一个占主导的重复行：三行交替重复的表格不该被当成循环。
	if g.output {
		top := 0
		for _, count := range g.counts {
			if count > top {
				top = count
			}
		}
		if top*100 < outputLoopMinTopSharePercent*size {
			return nil
		}
	}
	return g.loopError(repeated, size)
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
