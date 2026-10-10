// ═══ 更新日志 ═══
// 2026-10-10：请求行加 cache= 列，区分「会话首次冷启动」与「同会话内前缀丢失」。
// 2026-09-26：单独记录最后一次尝试的上游输入 token，供输出预算按会话推算（累计值会因重试偏大）。
// 2026-09-25：重试失败保留当次真实消费与错误，循环截断用量明确标记未完整上报。
// 2026-09-25：仅从实际 HTTP 尝试起点标记上游消费，发送前本地失败不再误入账本。
// 2026-09-25：结束原因独立解析，异常 choice 元数据不能丢掉同帧已知消费。
// 2026-09-25：每次上游观测同时写入请求尝试明细，额外保留真实思考计量和结束原因。
// 2026-09-25：前置计量读取与协议解析共用有界 SSE 行读取，事件累计含换行限制为 64MiB，防止先分配后检查。
// 2026-09-16：统计读取器保留底层错误，避免带末尾数据的断流被误报为正常 EOF。
// 2026-09-17：请求行加 key= 列（调用方密钥身份），并带上 prompt/completion 明细供用量账本记账。
// 2026-09-18：请求行加 in=（输入 tokens）与 hit=（其中缓存命中）两列，账本同步记录缓存维度：
//
//	思考模式下每轮都要重发整段上下文，只看 tok= 会让人觉得"用量明明很大却记了这么点"。
//
// 2026-09-19：流式与聚合共用原始用量观测，失败仍保留已知数值；按完整事件合并分帧字段，缺失显示 -。
// 2026-09-19：请求日志保留完整模型名，仅转义控制字符和表格分隔符以保持日志结构。
// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/runlog"
	"workbuddy2api/internal/upstream"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	requestID string
	start     time.Time
	model     string
	mode      string // "stream" | "sync"
	uid       string // 完整 uid，展示时只取前 8 位
	ttfb      time.Duration
	toks      int // <0 表示 usage 缺失 → 显示 "-"
	status    int

	// 调用方密钥身份（鉴权命中时填，单密钥模式留空 → 显示 "-"）。
	keyID      string
	keyName    string
	keyMask    string
	prompt     int
	lastPrompt int // 最后一次上报用量的尝试的输入 token（多次尝试不累加；0 = 未知）
	cached     int // 输入里命中提示缓存的 token 数（<0 表示未知）
	// sessKey 会话粘性键（已按调用方隔离）；cachePrev 是同一会话上**上一次**请求的
	// 输入 token（0 = 本进程没见过该会话 → 首次冷启动）。两者只用于日志行的 cache=
	// 列：`hit=0` 本身分不清「首次冷启动（正常）」与「同会话内前缀丢失（异常）」，
	// 有上一轮输入量做参照才能一眼判定（见 cacheField）。
	sessKey         string
	cachePrev       int
	hasUsage        bool
	credit          float64
	hasCred         bool
	upstreamStarted bool
	failed          bool
	unreported      bool

	logged       bool
	trace        *requestTrace
	attemptError string
}

// keyLabel 请求行里的密钥标识：优先名字，其次掩码密钥，都没有则 "-"。
func (s *chatStat) keyLabel() string {
	if s == nil {
		return "-"
	}
	if s.keyName != "" {
		return s.keyName
	}
	if s.keyMask != "" {
		return s.keyMask
	}
	return "-"
}

// sessionPrefixTracker 记录每个会话上一次请求的输入 token 数。
//
// 为什么需要：日志行的 `hit=` 只说「命中了多少」，**不说这次该不该命中**。排查
// 「同一会话后续请求突然不命中」时，`hit=0` 有两个完全不同的成因——首次冷启动
// （正常，前缀本来就没写过）与同会话内前缀丢失（异常，上一轮明明刚写过）。两者
// 在日志里长得一模一样，只能靠人工翻上一条请求去比。有了上一轮输入量做参照，
// `hit=0` 配上 `prev=546k` 一眼就是异常，配上 `cold` 就是正常。
//
// 有界：键来自客户端（会话标识），必须设上限，否则随机会话 ID 能把内存推成无上限。
// 超限按插入顺序淘汰最旧的（近似 LRU，精度够用——这里只为日志标注服务，不参与
// 任何路由或计费决策）。
type sessionPrefixTracker struct {
	mu    sync.Mutex
	prev  map[string]int
	order []string
}

const sessionPrefixMax = 4096

var sessionPrefixes = &sessionPrefixTracker{prev: make(map[string]int, sessionPrefixMax)}

// last 返回该会话上一次请求的输入 token；从未见过返回 0。
func (t *sessionPrefixTracker) last(key string) int {
	if key == "" {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.prev[key]
}

// record 记下该会话本次的输入 token（<=0 视为未知，不覆盖已有值）。
func (t *sessionPrefixTracker) record(key string, prompt int) {
	if key == "" || prompt <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.prev[key]; !seen {
		if len(t.order) >= sessionPrefixMax {
			// 淘汰最旧的一条，避免无界增长。
			oldest := t.order[0]
			t.order = t.order[1:]
			delete(t.prev, oldest)
		}
		t.order = append(t.order, key)
	}
	t.prev[key] = prompt
}

// cacheField 生成日志行的 cache= 列：把「本次命中」放到「上一轮输入」的参照系里。
//   - `-`            输入未知（上游没报 usage），无法判断
//   - `cold`         本会话首次请求（没有上一轮可复用），hit=0 属正常
//   - `prev=<n>`     上一轮输入 n token；与 hit= 直接比较即可判定是否丢失
func (s *chatStat) cacheField() string {
	if s == nil || s.prompt < 0 {
		return "-"
	}
	if s.cachePrev <= 0 {
		return "cold"
	}
	return "prev=" + strconv.Itoa(s.cachePrev)
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1,
		prompt: -1, cached: -1, failed: true}
}

func (s *chatStat) observeAttempt(event upstream.ChatAttemptEvent) {
	if event.Stage == "start" {
		s.upstreamStarted = true
	}
	s.trace.event(event)
}

// absorbUsage is called once for each upstream attempt. The request itself is
// recorded once, while known consumption from distinct attempts is preserved.
func (s *chatStat) absorbUsage(observation *chatStatsReader) {
	if observation == nil {
		s.unreported = true
		code := s.attemptCode()
		if code == "" {
			code = "transport_error"
		}
		s.trace.observe(nil, code)
		s.attemptError = ""
		return
	}
	addKnown := func(current *int, value int) {
		if value < 0 {
			return
		}
		if *current < 0 {
			*current = 0
		}
		*current += value
	}
	addKnown(&s.prompt, observation.PromptTokens())
	if prompt := observation.PromptTokens(); prompt > 0 {
		s.lastPrompt = prompt
	}
	if tokens, ok := observation.Tokens(); ok {
		addKnown(&s.toks, tokens)
	}
	addKnown(&s.cached, observation.CachedTokens())
	if credit, ok := observation.Credit(); ok {
		s.credit += credit
		s.hasCred = true
	}
	s.hasUsage = s.hasUsage || observation.hasUsage
	code := s.attemptCode()
	s.unreported = s.unreported || !observation.CompleteUsage() || traceLoopCode(code)
	s.trace.observe(observation, code)
	s.attemptError = ""
	if s.mode == "stream" && s.ttfb == 0 {
		s.ttfb = observation.TTFB()
	}
}

func (s *chatStat) absorbJSONUsage(body []byte) {
	if s.trace != nil && s.trace.pending != nil && s.trace.pending.HTTPStatus >= 400 {
		s.attemptError = traceHTTPCode(s.trace.pending.HTTPStatus, body)
	}
	observation := newChatStatsReaderSince(strings.NewReader(""), s.start)
	observation.observeJSON(string(body))
	s.absorbUsage(observation)
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	// 先取上一轮的参照值，再记录本轮——顺序反了就会把自己当成「上一轮」。
	s.cachePrev = sessionPrefixes.last(s.sessKey)
	sessionPrefixes.record(s.sessKey, s.prompt)
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status,
		s.prompt, s.cached, s.toks, s.keyLabel(), s.cacheField(), s.requestID)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br             *bufio.Reader
	start          time.Time
	ttfb           time.Duration
	seen           bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage       bool // 末帧是否带 usage
	hasCredit      bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens         int
	credit         float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt         int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cached         int     // usage.prompt_cache_hit_tokens / prompt_tokens_details.cached_tokens
	pend           string  // 已读未返回的行缓存；复用有界行，避免额外复制整行
	readErr        error
	data           strings.Builder
	hasData        bool
	eventNameBytes int
	maxEventBytes  int // zero selects the shared upstream SSE limit; small values support boundary tests
	reasoning      *int64
	finishReason   string
	sawDone        bool
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since, tokens: -1, prompt: -1, cached: -1}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) {
	if s.tokens < 0 {
		return 0, false
	}
	return s.tokens, true
}

// PromptTokens 返回已观测的 usage.prompt_tokens，缺失为 -1。
func (s *chatStatsReader) PromptTokens() int { return s.prompt }

// CachedTokens 返回末帧 usage 里「输入缓存命中」的 token 数。
// 上游用 prompt_cache_hit_tokens 报这个值，OpenAI 形状的响应放在 prompt_tokens_details.cached_tokens；
// 两者都没有时返回 -1（未知 ≠ 0，账本据此区分「没命中」与「没观测」）。
func (s *chatStatsReader) CachedTokens() int {
	if s.cached < 0 {
		return -1
	}
	return s.cached
}

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return max(0, s.prompt) + max(0, s.tokens) }

func (s *chatStatsReader) CompleteUsage() bool { return s.prompt >= 0 && s.tokens >= 0 }

func (s *chatStatsReader) responseEnded() bool {
	return s.sawDone || (s.finishReason != "" && s.readErr == io.EOF)
}

func (s *chatStatsReader) eventLimit() int {
	if s.maxEventBytes > 0 {
		return s.maxEventBytes
	}
	return upstream.MaxSSEEventBytes
}

func oversizedStatsEvent() error {
	return &upstream.StreamError{Code: "upstream_event_too_large", Message: "upstream stream line or event exceeded the size limit"}
}

// parseSSELine shares SSE data/newline and event-name accounting with the
// downstream protocol parser. Empty data fields consume a separator byte too;
// storing a slice of strings would otherwise allocate unbounded headers.
func (s *chatStatsReader) parseSSELine(line string) error {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		s.observePendingEvent()
		return nil
	}
	field, payload, found := strings.Cut(line, ":")
	if found {
		payload = strings.TrimPrefix(payload, " ")
	}
	if field == "event" {
		if len(payload) > s.eventLimit()-s.data.Len() {
			return oversizedStatsEvent()
		}
		s.eventNameBytes = len(payload)
		return nil
	}
	if field != "data" {
		return nil
	}
	extra := len(payload)
	if s.hasData {
		extra++
	}
	if extra > s.eventLimit()-s.data.Len()-s.eventNameBytes {
		return oversizedStatsEvent()
	}
	if !s.seen && payload != "[DONE]" {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	if s.hasData {
		s.data.WriteByte('\n')
	}
	s.data.WriteString(payload)
	s.hasData = true
	return nil
}

func (s *chatStatsReader) resetPendingEvent() {
	s.data.Reset()
	s.hasData = false
	s.eventNameBytes = 0
}

func (s *chatStatsReader) observePendingEvent() {
	if s.hasData {
		s.observeJSON(s.data.String())
	}
	s.resetPendingEvent()
}

func (s *chatStatsReader) observeJSON(payload string) {
	if strings.TrimSpace(payload) == "[DONE]" {
		s.sawDone = true
		return
	}
	var chunk struct {
		Usage   map[string]any  `json:"usage"`
		Choices json.RawMessage `json:"choices"`
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	if decoder.Decode(&chunk) != nil {
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		return
	}
	// Output metadata is not a prerequisite for metering. A malformed choice
	// must still fail protocol validation, but cannot erase valid usage beside it.
	var choices []struct {
		FinishReason json.RawMessage `json:"finish_reason"`
	}
	if json.Unmarshal(chunk.Choices, &choices) == nil {
		for _, choice := range choices {
			var reason string
			if json.Unmarshal(choice.FinishReason, &reason) == nil && reason != "" {
				s.finishReason = traceCode(reason)
			}
		}
	}
	if chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	if value, ok := upstream.UsageCount(chunk.Usage["completion_tokens"]); ok {
		s.tokens = value
	}
	if value, ok := upstream.UsageCount(chunk.Usage["prompt_tokens"]); ok {
		s.prompt = value
	}
	if value, ok := upstream.CachedInputTokens(chunk.Usage); ok {
		s.cached = value
	}
	if value, ok := upstream.UsageCredit(chunk.Usage["credit"]); ok {
		s.hasCredit = true
		s.credit = value
	}
	if value, ok := upstream.UsageCount(chunk.Usage["completion_thinking_tokens"]); ok {
		n := int64(value)
		s.reasoning = &n
	} else if details, ok := chunk.Usage["completion_tokens_details"].(map[string]any); ok {
		if value, valid := upstream.UsageCount(details["reasoning_tokens"]); valid {
			n := int64(value)
			s.reasoning = &n
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	if s.readErr != nil {
		s.resetPendingEvent()
		return 0, s.readErr
	}
	line, err := upstream.ReadSSELine(s.br, s.eventLimit())
	if line != "" {
		if parseErr := s.parseSSELine(line); parseErr != nil {
			s.resetPendingEvent()
			s.readErr = parseErr
			return 0, parseErr
		}
		if err == io.EOF {
			s.observePendingEvent()
		}
		s.readErr = err
		s.pend = line
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	if err == io.EOF {
		s.observePendingEvent()
	} else if err != nil {
		s.resetPendingEvent()
	}
	s.readErr = err
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := upstream.UsageCount(u["completion_tokens"])
	if !ok {
		return -1
	}
	return v
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := upstream.UsageCredit(u["credit"])
	pt, hasPrompt := upstream.UsageCount(u["prompt_tokens"])
	ct, hasCompletion := upstream.UsageCount(u["completion_tokens"])
	if !hasCredit || !hasPrompt || !hasCompletion {
		return 0, 0, false
	}
	return c, pt + ct, true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
//
// 三个 token 列都是「上游 usage 原值」：in= 输入、hit= 输入里命中缓存的、
// tok= 输出（含思考 token）。负值表示上游没给 usage，显示 "-"（缺失≠0）。
// cache= 列给出「本次该不该命中」的参照（cold / prev=N），见 chatStat.cacheField。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status, prompt, cached, toks int, key, cache string, requestIDs ...string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = strings.NewReplacer("\r", "\\r", "\n", "\\n", "\t", "\\t", "|", "\\u007c").Replace(model)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	promptField := "-"
	if prompt >= 0 {
		promptField = fmt.Sprintf("%d", prompt)
	}
	cachedField := "-"
	if cached >= 0 {
		cachedField = fmt.Sprintf("%d", cached)
	}
	if cache == "" {
		cache = "-"
	}
	suffix := ""
	if len(requestIDs) > 0 && requestIDs[0] != "" {
		suffix = " rid=" + strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
				return r
			}
			return -1
		}, requestIDs[0]) + " |"
	}
	fmt.Fprintf(runlog.Output(os.Stdout), "| #%03d | %s | %s | %s | %d | key=%s | uid=%s | TTFB=%s | in=%s | hit=%s | cache=%s | tok=%s | %stok/s | total=%.1fs |%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		key,
		uidPrefix(uid),
		ttfbMS,
		promptField,
		cachedField,
		cache,
		tokField,
		tokpsField,
		total.Seconds(),
		suffix,
	)
}
