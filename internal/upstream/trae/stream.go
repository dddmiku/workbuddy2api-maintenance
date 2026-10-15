// stream.go — Trae llm_utils_chat 的 SSE 事件流 → OpenAI chat.completions SSE。
//
// 这是嵌入 Trae2api-cn 的核心：上游发的是 Trae 私有事件族（output / token_usage /
// done），下游整条协议栈（Chat / Anthropic / Responses / 面板 / 池 / 粘性）只认
// OpenAI SSE。转换器把私有事件归一成标准帧，于是 Trae 账号可以和既有 realm
// 一样复用全部下游能力——这正是"模型使用相关用我们写好的模式"的落点。
//
// 移植自 Trae2api-cn 的 sse.py（translate_ide_stream 及其累加器）。保留的语义：
//   - 文本可能是**累计快照**也可能是**增量**：两者都按前缀合并（add）；
//   - 工具调用按 index 跨帧合并（首片带 id/name，后续只带参数片段）；
//   - `<think>…</think>` 从正文里拆出来当 reasoning，不混进回答；
//   - 没有终态事件就是截断（require_terminal），不能让半截回答伪装成功；
//   - 上游自造的 finish_reason 归一（对齐网关既有的 validFinishReason 口径）。
//
// ═══ 更新日志 ═══
// 2026-10-11：初版——事件→OpenAI SSE 转译 + 工具累加器 + 终态校验。
package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstreamerr"
)

// 上游事件名（Trae llm_utils_chat）。
const (
	evOutput     = "output"
	evTokenUsage = "token_usage"
	evDone       = "done"
	evError      = "error"
	// 远程/CLI 通道会用这些名字发心跳，保持连接不静默。
	evHeartbeat = "heartbeat"
)

// UpstreamError 来自叶子包 upstreamerr——定义在本包会与 upstream 包形成循环依赖
// （upstream import trae）。别名让 trae 包内的构造点读起来自然。
//
// 注意**不要**在本包再导出 RateLimitCode：credits.go 已有同名的 9074 常量
// （签到限流），两者是不同上游接口的不同码，同名会让调用点看不出用的是哪个。
// 流内 3004 一律走 upstreamerr.RateLimitCode。
type UpstreamError = upstreamerr.Error

// thinkOpen/thinkClose 是网关侧把 reasoning 包回正文的标记（对齐 Trae2api-cn）。
const (
	thinkOpen  = " thinking\n\n"
	thinkClose = "\n response\n\n"
)

var chunkSeq uint64

// ChatStream 调用 llm_utils_chat 并返回**已转译成 OpenAI SSE** 的流。
//
// 返回的 io.ReadCloser 与既有 upstream.ChatStreamContext 契约一致：调用方直接
// 交给 upstream.Stream / Aggregate，无需知道上游是 Trae。上游非 2xx 时返回
// (nil, status, body, nil) 让上层按既有错误分类处理（对齐既有出站实现）。
func ChatStream(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := newRequestID()
	// 用实时目录把客户端模型名解析成 config_name（含显示名匹配，如
	// "DeepSeek-V4.1-Flash" → "deepseek-v4.1-flash"）；目录不可用时
	// BuildChatBody 内部回落静态别名表。
	if models, cerr := FetchCatalog(ctx, a); cerr == nil && len(models) > 0 {
		if resolved := ResolveFromCatalog(models, modelFromPrepared(prepared)); resolved != "" {
			prepared = withModel(prepared, resolved)
		}
	}
	body, err := BuildChatBody(prepared, requestID)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBase+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	headers := buildHeaders(a)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	client := &http.Client{Transport: chatTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	stream := TranslateStream(resp.Body, modelFromPrepared(prepared))
	// 工具轮次超限（3004）时透明回落到 Remote 会话端点。回落发生在**客户端收到
	// 任何内容之前**才安全——已交付内容再换端点会让用户看到重复/断裂的输出。
	// 触发条件与代价见 remote.go 文件头。
	if RemoteEnabled() {
		stream = newRemoteFallbackReader(ctx, a, prepared, clientIP, stream)
	}
	return stream, resp.StatusCode, nil, nil
}

// remoteFallbackReader 在 llm_utils_chat 因工具轮次超限被拒时，透明切到 Remote 端点。
//
// 为什么在这一层做而不是在 handler：3004 是 trae 域自己的端点限制，与网关的
// 选号/冷却/轮转逻辑无关（换号改不了请求体）。放在本包内，上层完全无感。
type remoteFallbackReader struct {
	ctx       context.Context
	auth      *auth.Auth
	prepared  []byte
	clientIP  string
	primary   io.ReadCloser
	secondary io.ReadCloser
	// sawContent 是否已向下游交付过**实质内容**（正文/思考/工具帧）。
	//
	// 判据刻意不含开头的 role 帧：translator 在开始转译时就发一帧
	// `{"delta":{"role":"assistant"}}`，上游在**首个事件**就发 error（3004）时，
	// 下游收到的唯一内容就是这一帧。它不携带任何正文，丢掉后由 Remote 重新开场
	// 完全无害（客户端只是多看到一个 role 标记）。
	// 反过来，一旦交付过正文就绝不能回落——那会让用户看到重复或断裂的输出。
	sawContent bool
	// buffered 尚未交给下游的字节（判定阶段先扣住，见 Read 的说明）。
	buffered []byte
	// released 判定已结束、进入「原样透传」模式。
	released bool
	switched bool
	done     bool
}

func newRemoteFallbackReader(ctx context.Context, a *auth.Auth, prepared []byte,
	clientIP string, primary io.ReadCloser) *remoteFallbackReader {
	return &remoteFallbackReader{ctx: ctx, auth: a, prepared: prepared,
		clientIP: clientIP, primary: primary}
}

// Read 的实现要点：**先攒够判定依据再下发**。
//
// 上游在首个事件就发 3004 时，translator 会先发一帧 role（不含正文），紧接着把错误
// 传给下游。若那一帧直接下发，回落就会晚一步、客户端会先看到一个孤立开场帧。
// 所以这里把「不含实质内容」的字节先扣在 buffered 里，直到出现下列任一情况才放行：
//   - 读到实质内容 → 连同已扣的一起交出（正常路径，回落不再可能）；
//   - 读到错误     → 3004 且未交付过内容则回落；否则把扣住的帧交出并报错。
//
// 用循环而不是递归：递归会重新进入「buffered 非空就下发」的分支，等于立刻放行，
// 判定形同虚设（2026-10-15 首版就是这么漏的，被 TestRemoteFallbackSwitchesOn3004 抓住）。
func (r *remoteFallbackReader) Read(p []byte) (int, error) {
	for {
		// 已切换：之后全部读自 Remote。
		if r.secondary != nil {
			n, err := r.secondary.Read(p)
			if n > 0 {
				r.sawContent = true
			}
			return n, err
		}
		// 已决定放行：先把扣住的字节吐完。
		if r.released {
			if len(r.buffered) > 0 {
				n := copy(p, r.buffered)
				r.buffered = r.buffered[n:]
				return n, nil
			}
			if r.done {
				return 0, io.EOF
			}
			n, err := r.primary.Read(p)
			if n > 0 {
				r.sawContent = true
			}
			if err != nil {
				r.done = true
			}
			return n, err
		}
		if r.done {
			return 0, io.EOF
		}
		// 判定阶段：从 primary 读，不含实质内容的字节先扣住。
		n, err := r.primary.Read(p)
		if n > 0 {
			if containsSubstantiveDelta(p[:n]) {
				r.sawContent = true
				r.released = true
				// 扣住的字节先交出去，本次读到的留给下一次。
				r.buffered = append(r.buffered, p[:n]...)
				n = copy(p, r.buffered)
				r.buffered = r.buffered[n:]
				return n, nil
			}
			r.buffered = append(r.buffered, p[:n]...)
			continue
		}
		if err == nil {
			continue
		}
		// 尚未交付实质内容 + 上游明确因工具轮次超限拒绝 → 切 Remote 重来一次。
		if !r.sawContent && isToolTurnLimitError(err) {
			secondary, ferr := ChatStreamRemote(r.ctx, r.auth, r.prepared, r.clientIP)
			if ferr == nil {
				log.Printf("INFO: [trae] llm_utils_chat 因工具轮次超限被拒（3004），已回落到 Remote 会话端点")
				_ = r.primary.Close()
				r.secondary = secondary
				r.buffered = nil
				r.switched = true
				continue
			}
			// 回落也失败：如实报告两边的原因，别只丢一个。
			log.Printf("WARN: [trae] Remote 回落失败：%v（原始错误：%v）", ferr, err)
			return 0, fmt.Errorf("trae: llm_utils_chat 被拒（%v），Remote 回落也失败：%w", err, ferr)
		}
		// 不回落：把扣住的开场帧交出去，然后正常报错。
		r.done = true
		if len(r.buffered) > 0 {
			n := copy(p, r.buffered)
			r.buffered = r.buffered[n:]
			if n > 0 {
				return n, nil
			}
		}
		return 0, err
	}
}

func (r *remoteFallbackReader) Close() error {
	err := r.primary.Close()
	if r.secondary != nil {
		if serr := r.secondary.Close(); serr != nil && err == nil {
			err = serr
		}
	}
	return err
}

// containsSubstantiveDelta 报告这批 OpenAI SSE 字节里是否含**实质内容**。
//
// 只认正文 / 思考 / 工具帧；role 帧与空 delta 不算（见 sawContent 注释）。
// 解析失败时保守地当作「有内容」——宁可放弃回落，也不能冒重复输出的风险。
func containsSubstantiveDelta(chunk []byte) bool {
	text := string(chunk)
	for {
		idx := strings.Index(text, "data: ")
		if idx < 0 {
			return false
		}
		text = text[idx+len("data: "):]
		end := strings.IndexByte(text, '\n')
		line := text
		if end >= 0 {
			line = text[:end]
			text = text[end+1:]
		} else {
			text = ""
		}
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			if text == "" {
				return false
			}
			continue
		}
		var frame struct {
			Choices []struct {
				Delta map[string]any `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil {
			// 解析不了（可能被截断）：保守当作有内容，不回落。
			return true
		}
		for _, choice := range frame.Choices {
			if len(choice.Delta) == 0 {
				continue
			}
			if content, ok := choice.Delta["content"].(string); ok && content != "" {
				return true
			}
			if reasoning, ok := choice.Delta["reasoning_content"].(string); ok && reasoning != "" {
				return true
			}
			if calls, ok := choice.Delta["tool_calls"].([]any); ok && len(calls) > 0 {
				return true
			}
		}
		if text == "" {
			return false
		}
	}
}

// isToolTurnLimitError 报告错误是否为「工具轮次超限」的 3004 拒绝。
//
// 判据用**业务码**而不是文案：文案是英文提示「exceeded the rate limit」，会随
// 上游改版漂移，且容易与真正的限流混淆（真正的限流是换号能解决的，这个是请求
// 结构问题——换号无效，只有换端点）。
func isToolTurnLimitError(err error) bool {
	var upstreamErr *upstreamerr.Error
	if !errors.As(err, &upstreamErr) {
		return false
	}
	return upstreamErr.Code == upstreamerr.RateLimitCode
}

// ChatStreamRemote 走 Remote 会话端点（两步协议），返回**已转译成 OpenAI SSE** 的流。
//
// 由上层在 llm_utils_chat 返回 3004（工具轮次超限）时回落调用——为什么需要它、
// 以及实测的触发条件与代价，见 remote.go 文件头。
//
// 与 ChatStream 的差别：这里**不返回上游状态码**给上层做错误分类，而是把两步协议
// 里的失败就地转成 error（建会话失败时上游给的是业务码，不是可分类的 HTTP 状态）。
func ChatStreamRemote(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := newRequestID()
	body, _, _, err := buildRemoteBody(prepared, requestID)
	if err != nil {
		return nil, err
	}
	session, err := createRemoteSession(ctx, a, body)
	if err != nil {
		return nil, err
	}
	events, err := openRemoteEvents(ctx, a, session)
	if err != nil {
		stopRemoteSession(context.WithoutCancel(ctx), a, session.id)
		return nil, err
	}
	// 收流结束/客户端断开后主动停会话：上游对会话有并发槽位，不结束会占着。
	return &remoteStreamCloser{
		ReadCloser: TranslateStream(translateRemoteEvents(events, modelFromPrepared(prepared)),
			modelFromPrepared(prepared)),
		auth:    a,
		session: session,
	}, nil
}

// remoteStreamCloser 在流关闭时顺带结束上游会话。
type remoteStreamCloser struct {
	io.ReadCloser
	auth    *auth.Auth
	session remoteSession
}

func (c *remoteStreamCloser) Close() error {
	err := c.ReadCloser.Close()
	// 用不带取消的 ctx：客户端断开时 ctx 已取消，但停会话的请求仍要发出去。
	stopRemoteSession(context.WithoutCancel(context.Background()), c.auth, c.session.id)
	return err
}

// RawChatStream 与 ChatStream 相同，但**不做转译**，直接返回上游原始 SSE。
//
// 只用于排障：工具调用的 name/arguments 具体落在哪个事件、哪个字段，只有看原始
// 事件才能确定（2026-10-11 实测发现上游把工具身份与参数拆在多个事件里）。
func RawChatStream(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestID := newRequestID()
	body, err := BuildChatBody(prepared, requestID)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBase+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	headers := buildHeaders(a)
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	client := &http.Client{Transport: chatTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// TranslateStream 把 Trae 上游事件流（llm_utils_chat 的 SSE）转成 OpenAI SSE 流。
//
// ChatStream 内部走这里；单独导出是因为跨包的契约测试需要喂**合成**事件流来验证
// 转换器的输出能被网关自己的解析器消费（internal/upstream 已 import 本包，
// 反向 import 会成环，所以那个测试只能待在 upstream 包里）。
func TranslateStream(upstream io.ReadCloser, model string) io.ReadCloser {
	return newTranslateReader(upstream, newTranslator(model))
}

// chatTransport 是 SSE 专用 Transport：首字节有超时、流中空闲交给上游事件心跳，
// 不设总时长（长回答不能因为整体耗时被砍断）。
//
// ResponseHeaderTimeout 300s（2026-10-14 从 120s 调大）：实测 trae 上游对大输入
// （53K token）的处理期会先发 progress_notice 帧再吐正文，头部 26s 内就到；但更大
// 请求（Claude Code 全量 283K token）的头部可能显著更晚。120s 的头部窗在长上下文
// 场景没有余量，先放宽到 300s，让「等得起」成为默认（流中空闲不受此参数约束）。
func chatTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
	}
}

func newRequestID() string {
	n := atomic.AddUint64(&chunkSeq, 1)
	return fmt.Sprintf("trae-%d-%d", time.Now().UnixNano(), n)
}

// withModel 把出站请求体的 model 字段换成解析后的 config_name。
// 解析失败或 body 不可解析时原样返回（不二次错误化）。
func withModel(prepared []byte, model string) []byte {
	var src map[string]any
	if json.Unmarshal(prepared, &src) != nil || src == nil {
		return prepared
	}
	if current, _ := src["model"].(string); current == model {
		return prepared
	}
	src["model"] = model
	out, err := json.Marshal(src)
	if err != nil {
		return prepared
	}
	return out
}

func modelFromPrepared(prepared []byte) string {
	var src map[string]any
	if json.Unmarshal(prepared, &src) != nil {
		return ""
	}
	model, _ := src["model"].(string)
	return model
}

// ── 文本累加器 ────────────────────────────────────────────────────────────

// textAccumulator 合并「累计快照」与「增量」两种上游形态。
//
// Trae 的 output 事件有时发完整快照（response 是到目前为止的全部文本），有时只发
// 新增片段，两种会在同一条流里混用。判定规则来自 Trae2api-cn 的实测：
//   - 新值以旧值开头 → 是更新的快照，替换；
//   - 旧值以新值开头 → 是迟到的旧快照，丢弃；
//   - 公共前缀够长（≥4 或超过较短者一半）→ 认为是对同一段的重写，替换；
//   - 否则 → 视为纯增量，追加。
type textAccumulator struct {
	raw     string
	visible string
}

// add 吸收一个新值，返回本次应当交付给客户端的**新增**可见文本。
func (t *textAccumulator) add(value string) string {
	if value == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(value, t.raw):
		t.raw = value
	case strings.HasPrefix(t.raw, value):
		// 旧快照，不改动。
	default:
		common := commonPrefixLen(t.raw, value)
		shorter := min(len(t.raw), len(value))
		if common >= 4 || (shorter > 0 && common*2 >= shorter) {
			t.raw = value
		} else {
			t.raw += value
		}
	}
	return t.delta()
}

// finalize 在流结束时放行最后一个可见增量（含尾部不完整转义前缀）。
func (t *textAccumulator) finalize() string { return t.delta() }

func (t *textAccumulator) delta() string {
	visible := visibleText(t.raw)
	if visible == t.visible {
		return ""
	}
	addition := textDelta(t.visible, visible)
	t.visible = visible
	return addition
}

// visibleText 剥掉序列化在正文里的工具调用块（XML 形态），并压住不完整的开头。
func visibleText(raw string) string {
	return holdIncompleteToolBlock(stripToolCallBlocks(raw))
}

// textDelta 返回 current 相对 previous 的新增部分。
func textDelta(previous, current string) string {
	if current == "" {
		return ""
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	if strings.HasPrefix(previous, current) {
		return ""
	}
	return current[commonPrefixLen(previous, current):]
}

func commonPrefixLen(left, right string) int {
	limit := min(len(left), len(right))
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}
	return index
}

// toolBlockPairs 是正文里可能出现的序列化工具块（上游把工具调用写进文本的形态）。
var toolBlockPairs = [][2]string{
	{"<opencode_tool_call>", "</opencode_tool_call>"},
	{"<tool_use>", "</tool_use>"},
	{"<tool_call", "</tool_call>"},
	{"<tool_cell", "</tool_cell>"},
	{"<invoke", "</invoke>"},
	{"<bash>", "</bash>"},
	{"<read>", "</read>"},
	{"<write>", "</write>"},
	{"<edit>", "</edit>"},
	{"<glob>", "</glob>"},
	{"<grep>", "</grep>"},
	{"<task>", "</task>"},
}

// stripToolCallBlocks 移除成对的工具块；未闭合的块由 holdIncompleteToolBlock 压住。
func stripToolCallBlocks(text string) string {
	out := text
	for _, pair := range toolBlockPairs {
		for {
			start := strings.Index(out, pair[0])
			if start < 0 {
				break
			}
			end := strings.Index(out[start+len(pair[0]):], pair[1])
			if end < 0 {
				break
			}
			out = out[:start] + out[start+len(pair[0])+end+len(pair[1]):]
		}
	}
	return out
}

// holdIncompleteToolBlock 截断到第一个「已开始但还没闭合」的工具块之前，
// 避免把半截工具标记当正文发给客户端。
func holdIncompleteToolBlock(text string) string {
	lowered := strings.ToLower(text)
	earliest := len(text)
	for _, pair := range toolBlockPairs {
		start := strings.Index(lowered, pair[0])
		if start < 0 {
			continue
		}
		if strings.Index(lowered[start+len(pair[0]):], pair[1]) < 0 {
			earliest = min(earliest, start)
		}
	}
	// 可能正好停在 "<tool_c" 这种前缀上：压住最后一个 '<' 起的尾巴。
	if last := strings.LastIndex(lowered, "<"); last >= 0 {
		suffix := lowered[last:]
		for _, pair := range toolBlockPairs {
			if strings.HasPrefix(pair[0], suffix) {
				earliest = min(earliest, last)
				break
			}
		}
	}
	return text[:earliest]
}

// ── 工具调用累加器 ────────────────────────────────────────────────────────

type toolCallState struct {
	id        string
	typ       string
	name      string
	arguments strings.Builder
	seenArgs  bool
}

// toolAccumulator 按 index 合并工具调用片段。
//
// Trae 原生形态：首片带 id/name，后续片段只带 arguments 增量、不带 id/name。
// 因此 index 是唯一稳定的合并键（Trae2api-cn 的 ToolCallAccumulator 结论）。
type toolAccumulator struct {
	order []int
	calls map[int]*toolCallState
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{calls: map[int]*toolCallState{}}
}

func (t *toolAccumulator) hasCalls() bool { return len(t.calls) > 0 }

// absorb 吸收一批工具片段，返回本帧要交付的 delta 列表（OpenAI 形状）。
func (t *toolAccumulator) absorb(items []any) []map[string]any {
	var deltas []map[string]any
	for fallback, raw := range items {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		index := fallback
		if value, ok := call["index"].(float64); ok {
			index = int(value)
		}
		state := t.calls[index]
		if state == nil {
			state = &toolCallState{}
			t.calls[index] = state
			t.order = append(t.order, index)
		}
		delta := map[string]any{"index": index}
		if id, _ := call["id"].(string); id != "" {
			if state.id == "" {
				state.id = id
				delta["id"] = id
			}
		}
		if typ, _ := call["type"].(string); typ != "" {
			if state.typ == "" {
				state.typ = typ
				delta["type"] = typ
			}
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			// 紧凑形态：name/arguments 直接放在调用对象上。
			fn = call
		}
		// 上游原生形态：name/arguments 在 `function_call` 里（不是 OpenAI 的
		// `function`）。2026-10-11 实测原始事件确认：
		//   {"index":0,"id":"call_...","type":"function",
		//    "function_call":{"name":"read_file","arguments":"{\""}}
		//   {"index":0,"id":"","type":"function",
		//    "function_call":{"name":"","arguments":"path\":\"/etc/hostname\"}"}}
		// 即工具身份与参数**拆在两个事件**里，且 arguments 是增量拼接。
		// 只认 `function` 会让 name 与参数全部丢失。
		if native, ok := call["function_call"].(map[string]any); ok {
			merged := map[string]any{}
			for k, v := range fn {
				merged[k] = v
			}
			for k, v := range native {
				merged[k] = v
			}
			fn = merged
		}
		fnDelta := map[string]any{}
		if name, _ := fn["name"].(string); name != "" && state.name == "" {
			state.name = name
			fnDelta["name"] = name
		}
		if args, ok := fn["arguments"].(string); ok && args != "" {
			// 参数可能是累计快照也可能是增量，与正文同一套合并规则。
			addition := args
			if existing := state.arguments.String(); existing != "" {
				addition = textDelta(existing, args)
				if addition == "" && strings.HasPrefix(existing, args) {
					continue
				}
			}
			state.arguments.WriteString(addition)
			state.seenArgs = true
			fnDelta["arguments"] = addition
		}
		if len(fnDelta) > 0 {
			delta["function"] = fnDelta
		}
		if len(delta) > 1 {
			deltas = append(deltas, delta)
		}
	}
	return deltas
}

// ── 转译器 ────────────────────────────────────────────────────────────────

type translator struct {
	model     string
	id        string
	text      *textAccumulator
	reasoning *textAccumulator
	tools     *toolAccumulator

	usage        map[string]any
	finishReason string
	sawTerminal  bool
	started      bool
	contentCount int
	// thinkingOpen 记录是否已经输出过 think 开头，用于把 reasoning 包成 think 块。
	thinkingOpen bool
	thinkingDone bool
}

func newTranslator(model string) *translator {
	return &translator{
		model:     model,
		id:        "chatcmpl-trae-" + newRequestID(),
		text:      &textAccumulator{},
		reasoning: &textAccumulator{},
		tools:     newToolAccumulator(),
	}
}

// emit 把一条 OpenAI SSE 帧写进输出缓冲。
func (t *translator) emit(out *bytes.Buffer, delta map[string]any, finish any) {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	frame := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{choice},
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return
	}
	out.WriteString("data: ")
	out.Write(encoded)
	out.WriteString("\n\n")
}

func (t *translator) start(out *bytes.Buffer) {
	if t.started {
		return
	}
	t.started = true
	t.emit(out, map[string]any{"role": "assistant"}, nil)
}

// handleEvent 处理一个上游事件，把产生的 OpenAI 帧写进 out。
// 返回 done=true 表示上游已给出终态，停止消费。
func (t *translator) handleEvent(name string, data []byte, out *bytes.Buffer) (bool, error) {
	name = normalizeEventName(name)
	var obj map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &obj); err != nil {
			// 事件名已经说明是终态时，载荷不合法也算收尾（对齐 Trae2api-cn）。
			if name == evDone {
				t.sawTerminal = true
				return true, nil
			}
			return false, nil
		}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	if name == "" {
		name = payloadEventName(obj)
	}

	switch name {
	case evError:
		message, _ := obj["message"].(string)
		// 上游把业务码放在 code 字段（实测 3004 = 限流/规模超限）。带类型返回，
		// 让上层能按码分流；丢了码就只能看到一句英文文案（见 UpstreamError 注释）。
		code := 0
		switch raw := obj["code"].(type) {
		case float64:
			code = int(raw)
		case json.Number:
			if n, err := raw.Int64(); err == nil {
				code = int(n)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				code = n
			}
		}
		return false, &UpstreamError{Code: code, Message: message}
	case evHeartbeat:
		// 心跳：保持连接活着，不产生内容。
		out.WriteString(": relay-keepalive\n\n")
		return false, nil
	case evTokenUsage:
		if usage, ok := obj["usage"].(map[string]any); ok {
			t.usage = mapUsage(usage)
		} else {
			t.usage = mapUsage(obj)
		}
		return false, nil
	case evDone:
		t.sawTerminal = true
		if reason := payloadFinishReason(obj); reason != "" {
			t.finishReason = reason
		}
		return true, nil
	}

	// output 事件（或没有事件名的 data 帧）：正文 + reasoning + 工具。
	reasoning := payloadReasoningText(obj)
	rawResponse, _ := obj["response"].(string)
	embeddedReasoning, visibleResponse := splitEmbeddedReasoning(rawResponse)

	// reasoning 先发：客户端要能区分"思考"与"回答"。
	for _, chunk := range []string{reasoning, embeddedReasoning} {
		if chunk == "" {
			continue
		}
		if addition := t.reasoning.add(chunk); addition != "" {
			t.start(out)
			t.emit(out, map[string]any{"reasoning_content": addition}, nil)
		}
	}
	if addition := t.text.add(visibleResponse); addition != "" {
		t.start(out)
		t.contentCount++
		t.emit(out, map[string]any{"content": addition}, nil)
	}
	if calls, ok := obj["tool_calls"].([]any); ok && len(calls) > 0 {
		for _, delta := range t.tools.absorb(calls) {
			t.start(out)
			t.emit(out, map[string]any{"tool_calls": []any{delta}}, nil)
		}
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		t.usage = mapUsage(usage)
	}
	if reason := payloadFinishReason(obj); reason != "" {
		// 中间快照也会带 finish_reason，那只是元数据；只有 done 才是终态。
		t.finishReason = reason
	}
	return false, nil
}

// finish 在流结束时补齐尾部增量、终态帧与 [DONE]。
func (t *translator) finish(out *bytes.Buffer) error {
	if !t.sawTerminal {
		return fmt.Errorf("Trae upstream ended before its terminal event")
	}
	// 尾部增量（可能含最后一段正文或 reasoning）。
	if addition := t.text.finalize(); addition != "" {
		t.start(out)
		t.contentCount++
		t.emit(out, map[string]any{"content": addition}, nil)
	}
	if addition := t.reasoning.finalize(); addition != "" {
		t.start(out)
		t.emit(out, map[string]any{"reasoning_content": addition}, nil)
	}
	if t.contentCount == 0 && !t.tools.hasCalls() {
		t.start(out)
		t.emit(out, map[string]any{"content": "(trae upstream returned an empty response)"}, nil)
	}
	reason := finalFinishReason(t.finishReason, t.tools.hasCalls())
	t.emit(out, map[string]any{}, reason)
	if t.usage != nil {
		usageFrame := map[string]any{
			"id":      t.id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   t.model,
			"choices": []any{},
			"usage":   t.usage,
		}
		if encoded, err := json.Marshal(usageFrame); err == nil {
			out.WriteString("data: ")
			out.Write(encoded)
			out.WriteString("\n\n")
		}
	}
	out.WriteString("data: [DONE]\n\n")
	return nil
}

// ── 事件字段抽取 ──────────────────────────────────────────────────────────

func normalizeEventName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "event:")
	value = strings.TrimSpace(value)
	value = strings.ToLower(value)
	return strings.ReplaceAll(value, "-", "_")
}

func payloadEventName(obj map[string]any) string {
	if value, ok := obj["event"].(string); ok {
		return normalizeEventName(value)
	}
	return evOutput
}

// payloadFinishReason 从多种拼写里取终态（Trae 各通道字段名不一致）。
func payloadFinishReason(obj map[string]any) string {
	for _, key := range []string{"finish_reason", "stop_reason", "finishReason", "stopReason"} {
		if value, ok := obj[key].(string); ok && value != "" {
			return value
		}
	}
	if message, ok := obj["message"].(map[string]any); ok {
		return payloadFinishReason(message)
	}
	return ""
}

// payloadReasoningText 取 reasoning 字段，不把它当正文。
func payloadReasoningText(obj map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoningContent", "thought", "thinking"} {
		switch value := obj[key].(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			for _, nested := range []string{"content", "text", "summary"} {
				if text, ok := value[nested].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	if message, ok := obj["message"].(map[string]any); ok {
		return payloadReasoningText(message)
	}
	return ""
}

// finalFinishReason 把上游终态归一成下游契约认识的值。
//
// 与网关既有的 validFinishReason 口径一致；上游自造的终止值（error/abort/…）
// 归一到 stop——正文已经交付，不该因为一个词不认识就制造失败。
func finalFinishReason(value string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	reason := strings.ToLower(strings.TrimSpace(value))
	switch reason {
	case "length", "max_tokens", "max_output_tokens", "max_token", "token_limit",
		"model_context_window_exceeded", "context_window_exceeded", "context_length_exceeded":
		return "length"
	case "content_filter", "content-filter", "safety":
		return "content_filter"
	}
	// stop / end_turn / eos / done / finished / complete，以及上游自造的
	// error/abort/canceled/failed/interrupted 与空值，一律按正常结束。
	return "stop"
}

// mapUsage 把 Trae 的 usage 归一成 OpenAI 形状。
func mapUsage(usage map[string]any) map[string]any {
	number := func(keys ...string) (int64, bool) {
		for _, key := range keys {
			if value, ok := usageCount(usage[key]); ok {
				return value, true
			}
		}
		return 0, false
	}
	out := map[string]any{}
	if input, ok := number("prompt_tokens", "input_token", "input_tokens", "promptTokenCount"); ok {
		out["prompt_tokens"] = input
	}
	if output, ok := number("completion_tokens", "output_token", "output_tokens", "candidatesTokenCount"); ok {
		out["completion_tokens"] = output
	}
	if total, ok := number("total_tokens", "totalTokenCount"); ok {
		out["total_tokens"] = total
	} else {
		in, _ := out["prompt_tokens"].(int64)
		oo, _ := out["completion_tokens"].(int64)
		out["total_tokens"] = in + oo
	}
	if cached, ok := number("cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens"); ok {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	// reasoning_tokens 下游按 completion_tokens_details.reasoning_tokens 读取
	// （见 server/responses.go 的 usageObject），归一过去思考量才不会丢。
	if reasoning, ok := number("reasoning_tokens", "completion_thinking_tokens"); ok {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func usageCount(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// ── <think> 拆分 ──────────────────────────────────────────────────────────

// splitEmbeddedReasoning 把正文里的 <think>…</think> 拆成 reasoning 与可见文本。
func splitEmbeddedReasoning(text string) (reasoning, visible string) {
	if text == "" {
		return "", ""
	}
	lowered := strings.ToLower(text)
	var parts []string
	var out strings.Builder
	index := 0
	for {
		open := indexOfAny(lowered[index:], "<think", "<thinking")
		if open < 0 {
			out.WriteString(text[index:])
			break
		}
		open += index
		// 找到开标签的 '>'
		tagEnd := strings.IndexByte(text[open:], '>')
		if tagEnd < 0 {
			out.WriteString(text[index:])
			break
		}
		tagEnd += open
		// 匹配的闭合标签与开标签同名
		tagName := "think"
		if strings.HasPrefix(lowered[open:], "<thinking") {
			tagName = "thinking"
		}
		closeTag := "</" + tagName + ">"
		close := strings.Index(lowered[tagEnd:], closeTag)
		if close < 0 {
			// 未闭合：整段留在可见文本里（由 holdIncompleteToolBlock 之外的原样透传）。
			out.WriteString(text[index:])
			break
		}
		close += tagEnd
		body := text[tagEnd+1 : close]
		if body != "" {
			parts = append(parts, body)
		}
		out.WriteString(text[index:open])
		index = close + len(closeTag)
	}
	return strings.Join(parts, "\n"), out.String()
}

func indexOfAny(haystack string, needles ...string) int {
	best := -1
	for _, needle := range needles {
		if at := strings.Index(haystack, needle); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

// ── 流式读取器 ────────────────────────────────────────────────────────────

// translateReader 把上游 Trae SSE 惰性转成 OpenAI SSE。
//
// 用 goroutine + io.Pipe 而不是全量缓冲：下游的 chatStatsReader 会边读边统计
// TTFB 与 usage，必须让字节尽快流出去，否则长回答的 TTFB 会被整段生成时间吃掉。
type translateReader struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	closer io.Closer
	done   chan struct{}
}

func newTranslateReader(upstream io.ReadCloser, tr *translator) *translateReader {
	pr, pw := io.Pipe()
	r := &translateReader{pr: pr, pw: pw, closer: upstream, done: make(chan struct{})}
	go r.run(upstream, tr)
	return r
}

func (r *translateReader) run(upstream io.ReadCloser, tr *translator) {
	defer close(r.done)
	var err error
	defer func() {
		_ = upstream.Close()
		_ = r.pw.CloseWithError(err)
	}()
	// 空闲心跳：上游长时间不吐帧时（大输入的处理期只有 progress_notice，正文
	// 要等模型开写），向下游发 SSE 注释行保活，防止中间层（nginx/CDN/客户端
	// 自身的读超时）在 17-60 秒量级把整条流判死（2026-10-14 trae 502 排查：
	// 客户端等不到任何字节是三连 502 的共同形态）。注释行按 SSE 规范会被
	// 客户端忽略，不污染内容流。
	//
	// 实现走 SetReadDeadline 轮询而非额外 goroutine：resp.Body 的 Read 阻塞
	// 在 deadline 上，超时返回 os.ErrDeadlineExceeded——这里把它当「该发心跳
	// 了」而不是错误，发完续期再读。真断连仍以 EOF/RST 呈现，不会被掩盖。
	deadlineAware, hasDeadline := upstream.(interface{ SetReadDeadline(time.Time) error })
	const idleBeat = 15 * time.Second
	beatDeadline := func() {
		if hasDeadline {
			_ = deadlineAware.SetReadDeadline(time.Now().Add(idleBeat))
		}
	}
	beatDeadline()
	defer func() {
		if hasDeadline {
			_ = deadlineAware.SetReadDeadline(time.Time{})
		}
	}()
	reader := bufio.NewReaderSize(upstream, 64*1024)
	var (
		out         bytes.Buffer
		eventName   string
		dataLines   []string
		sawUpstream bool
		terminal    bool
	)
	// flush 交付一个完整事件。terminal=true 表示上游已给出终态（done / [DONE]），
	// 停止消费但**不能**就此返回——尾部增量与终态帧还要在 finish 里补齐。
	flush := func() bool {
		if len(dataLines) == 0 {
			eventName = ""
			return false
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		name := eventName
		eventName = ""
		sawUpstream = true
		if strings.TrimSpace(payload) == "[DONE]" {
			tr.sawTerminal = true
			return true
		}
		done, herr := tr.handleEvent(name, []byte(payload), &out)
		if herr != nil {
			err = herr
			return true
		}
		if out.Len() > 0 {
			if _, werr := r.pw.Write(out.Bytes()); werr != nil {
				err = werr
				return true
			}
			out.Reset()
		}
		return done
	}
	for {
		line, rerr := reader.ReadString('\n')
		if rerr != nil && errors.Is(rerr, os.ErrDeadlineExceeded) {
			// 读截止到期 = 上游空闲超过 idleBeat：向下游补一个心跳注释行，
			// 然后续期再读。不 break、不置 err——空闲不是流错误（见 run 头部注释）。
			if _, werr := r.pw.Write([]byte(": relay-keepalive\n\n")); werr != nil {
				err = werr
				break
			}
			beatDeadline()
			continue
		}
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if flush() {
					terminal = true
				}
			case strings.HasPrefix(trimmed, ":"):
				// 上游注释行：转成下游心跳，客户端不会把它当内容。
				if _, werr := r.pw.Write([]byte(": relay-keepalive\n\n")); werr != nil {
					err = werr
					terminal = true
				}
			default:
				field, value, found := strings.Cut(trimmed, ":")
				if !found {
					continue
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					eventName = value
				case "data":
					dataLines = append(dataLines, value)
				}
			}
		}
		if terminal {
			break
		}
		if rerr != nil {
			if rerr == io.EOF {
				flush()
			} else {
				err = rerr
			}
			break
		}
	}
	if err != nil {
		return
	}
	if !sawUpstream {
		err = fmt.Errorf("Trae upstream returned no data events")
		return
	}
	// 尾部增量 + 终态帧 + [DONE]；缺终态时 finish 会返回截断错误。
	if ferr := tr.finish(&out); ferr != nil {
		err = ferr
		return
	}
	if out.Len() > 0 {
		if _, werr := r.pw.Write(out.Bytes()); werr != nil {
			err = werr
			return
		}
	}
}

func (r *translateReader) Read(p []byte) (int, error) { return r.pr.Read(p) }

func (r *translateReader) Close() error {
	_ = r.pr.Close()
	<-r.done
	return nil
}
