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

// ChatStream 调用上游并返回**已转译成 OpenAI SSE** 的流。
//
// 返回的 io.ReadCloser 与既有 upstream.ChatStreamContext 契约一致：调用方直接
// 交给 upstream.Stream / Aggregate，无需知道上游是 Trae。上游非 2xx 时返回
// (nil, status, body, nil) 让上层按既有错误分类处理（对齐既有出站实现）。
//
// ═══ 路径次序（2026-10-16 反转，对齐参考 UPSTREAM_MODE=remote）═══
//
// **Remote 会话端点是主路径**，llm_utils_chat 是回落：
//   - Remote 的 query 是扁平化文本（工具调用转文本标记），天然绕开 3004
//     工具轮次硬限制；solo_agent_remote 还保有 1M max 档（见 remote.go 文件头）；
//   - Remote 失败（建会话/收流/首事件超时）时回落 llm_utils_chat——两个端点
//     的故障域不同（Remote 是网页端会话服务，llm_utils_chat 是 IDE 通道），
//     互为冗余；
//   - Remote 关闭（WB2API_TRAE_REMOTE=0，排障用）时直接走 llm_utils_chat，
//     并保留其 3004 → Remote 的旧回落（那个方向不消失，只是次序反转）。
//
// 回落必须发生在**客户端收到任何内容之前**——已交付内容再换端点会让用户看到
// 重复/断裂的输出。Remote 建会话失败即满足（一个字节都还没发）。
// ChatStream 是 Trae 域的入口：优先 Remote 两步协议，失败回落 llm_utils_chat。
//
// ═══ 带图请求直接走 llm_utils_chat（2026-10-16 实测）═══
//
// Remote 端点**读不到内联图片**：把媒体块放进 query 的 JSON 数组、放进
// initial_message.content（参考 build_web_content 的原形）、或两者都放，上游一律
// 回「没有收到图片」——连目录里标了 multimodal:true 的 kimi-k2.6 / Doubao-Seed-2.1-Pro
// 也一样（三组对照实验，图片用 384×384 有结构的 PNG）。
//
// 同一个二进制、同一张图，llm_utils_chat 路径答得完全正确：
//
//	Doubao-Seed-2.1-Pro → "有圆形和长方形，背景是蓝色。"
//	kimi-k2.6           → "圆形、长方形；蓝色背景。"
//
// 所以带图请求**必须**走 legacy——那里 messages 是原生块数组，图片原样透传上游。
// 判据用「请求体里有没有图片块」，不是模型名：客户端可能给文本模型发图，让上游
// 按自己的口径拒绝比网关猜一个替代模型更诚实。
func ChatStream(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// ═══ 已移除「带图 + 非多模态模型 → 出站前拦下」（2026-10-16 二次修正）═══
	//
	// v3.8.4 加过一段拦截：读目录的 multimodal 标记，为 false 且本轮带图就在出站前
	// 拒绝，理由是「glm-5.3 没有视觉能力，上游只会回一句无关的英文提示」。
	//
	// 用户随后用 **Trae 客户端**（同一账号、同一个 GLM-5.3 Max）贴图提问，
	// 得到的是逐项准确的图片描述——「三角洲行动」「S1B+ 段位」「企鹅图案徽章」
	// 全认出来了。这直接推翻「该模型没有视觉能力」这个结论。
	//
	// 复核拦截依据时还发现它本身不可靠：判据来自目录接口的 multimodal 字段，
	// 而实测该接口对这几个 trae 账号返回的是**空列表**（
	// `{"allow_tenant_user_add_model":true,"config_info_list":[],"metadata":{}}`），
	// 也就是说那个 false 要么来自过期缓存、要么来自解析错误，不是上游当下的口径。
	//
	// 所以这里回到最朴素的策略：**不替上游判断模型能力**。带图请求照常发出去，
	// 上游认就给答案、不认就回它自己的错误——那比我用一个可疑的标记提前拒绝更诚实，
	// 也不会把本来能读图的组合拦掉。真正需要防的只是「回落丢图」，那条闸门仍在
	// （见 chatStreamLegacy 的 freshInputHasImages）。
	// ═══ 上下文压不进 Remote 上限时直接走 llm_utils_chat（2026-10-19 实测）═══
	//
	// 生产事故（09:26）：Claude Code 切到 trae:deepseek-v4.1-flash 后连吃 502。
	// 日志证据：
	//
	//	[trae] 模型 deepseek-v4.1-flash 默认 1M 档，执行档 solo_work_remote → solo_agent_remote
	//	[trae] Remote 收流已建立 session=… message=…     ← 建会话成功
	//	WARN: Trae upstream returned no data events（已收 0 字节）  ← 事件流空结束
	//
	// 逐层拆开那个 2,429,895 字节的 remote create body：query 1,101,077 字符，
	// content 1,100,958 字符，两者内容重叠。再拆 query 的段落，只有 **2 段**，
	// 且都是 system：
	//
	//	[System] You are Trae Code …        934,956 字符
	//	[System] x-anthropic-billing-header: …  52,051 字符
	//
	// 即 Claude Code 的上下文几乎全是 system 提示。而 boundedRemoteQuery 的裁剪
	// 循环只在「非 system 消息 > 1 条」时才删（它按对话轮次成对丢弃），全 system
	// 的请求 nonSystem==0，循环第一次判断就 return，一个字符都没裁掉。
	// 987K 字符的 query 远超 remoteQueryMaxChars=460_000，上游按既有实测口径
	// （>500K 静默断流）直接空结束，于是 502。
	//
	// 同一个 body 走 llm_utils_chat 是通的：实测 1,105,264 字节 / 204,033
	// prompt_tokens 返回 200（27.4s）。该端点的输入上限就是模型窗口 1M，
	// 没有 Remote 那种扁平化 query 的字符上限。
	//
	// 所以判据是「压不进 Remote 的 query 上限」而不是「大不大」：能压就仍走
	// Remote（它有 1M max 档与网页端会话能力），压不进才换端点。
	if RemoteEnabled() && !requestHasImages(prepared) && !remoteQueryUnbounded(prepared) {
		if stream, err := ChatStreamRemote(ctx, a, prepared, clientIP); err == nil {
			return stream, http.StatusOK, nil, nil
		} else {
			log.Printf("WARN: [trae] Remote 主路径失败，回落 llm_utils_chat：%v", err)
		}
	}
	return chatStreamLegacy(ctx, a, prepared, clientIP)
}

// remoteQueryUnbounded 报告这个请求的扁平化 query 是否**压不进** Remote 的上限。
//
// 只有一种情况压不进：裁剪后仍超限。boundedRemoteQuery 按对话轮次成对丢弃
// 非 system 消息，当请求里几乎没有非 system 消息时（典型是 Claude Code 把
// 整个环境说明塞进 system），它无物可删，只能原样返回。
//
// 这里复用与出站完全相同的三步（prepareRemoteMessages → compactRemoteHistory
// → boundedRemoteQuery），所以判据不会与真正发出去的 body 漂移。
//
// 代价：每次 Remote 请求多算一次扁平化（对 1MB 量级是毫秒级字符串操作），
// 换来的是把「必然 502」提前变成一个可用的端点选择。
func remoteQueryUnbounded(prepared []byte) bool {
	if len(prepared) == 0 {
		return false
	}
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return false
	}
	rawMessages, _ := src["messages"].([]any)
	if len(rawMessages) == 0 {
		return false
	}
	messages := make([]map[string]any, 0, len(rawMessages))
	for _, raw := range rawMessages {
		if m, ok := raw.(map[string]any); ok {
			messages = append(messages, m)
		}
	}
	prepared2, _ := prepareRemoteMessages(messages, src)
	compacted := compactRemoteHistory(prepared2, remoteHistoryMaxMessages, remoteHistoryMaxChars)
	bounded, _ := boundedRemoteQuery(compacted, remoteQueryMaxChars)
	return flattenQueryChars(bounded) > remoteQueryMaxChars
}

// freshInputHasImages 报告「本轮新输入」里是否带图片。
//
// 轮次边界是**上一条不含工具调用的 assistant 消息**——那才是上一轮的回答；
// 带 tool_calls 的 assistant 消息属于**本轮的中间步骤**，不是边界。
//
// 为什么边界要这样定（两个都踩过的坑）：
//  1. 不能取「最后一条消息」：Claude Code 的工具轮形态是
//     [… 用户贴图, assistant(tool_calls), tool(结果)]，最后一条是 tool_result
//     （role=user 但无图），只看最后一条会把用户刚贴的图误判成「无图」；
//  2. 不能把带 tool_calls 的 assistant 当边界：那会让图片落在边界之前，
//     同样误判成「无图」。
//
// 与 requestHasImages 的分工（2026-10-16 回归修复）：
//   - requestHasImages（扫全部消息）用于**路由**：历史里有图也该走 legacy，
//     因为它把 messages 原样透传，图片不会丢；
//   - 本函数用于**回落闸门**：只有本轮新输入带图才值得挡住回落。
//
// 为什么必须区分（v3.8.2 的教训）：回落闸门最初也用 requestHasImages，结果
// 「会话历史里曾经贴过一张截图」就永久禁用了该会话的 3004 回落——用户正常提问
// 直接吃 400，比丢一张旧图严重得多。实测事故：Claude Code 会话里带过截图后，
// trae:glm-5.3 连续三次 400，日志里没有任何回落记录。
func freshInputHasImages(prepared []byte) bool {
	if len(prepared) == 0 {
		return false
	}
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return true
	}
	messages, _ := src["messages"].([]any)
	// 从末尾往前扫，遇到「不含工具调用的 assistant 消息」即停——它之后的就是本轮。
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "assistant") && !hasToolCalls(message) {
			break
		}
		if messageHasImageBlock(message) {
			return true
		}
	}
	return false
}

// hasToolCalls 报告 assistant 消息是否携带工具调用（决定它是不是轮次边界）。
func hasToolCalls(message map[string]any) bool {
	for _, key := range []string{"tool_calls", "function_call"} {
		if value, present := message[key]; present && value != nil {
			switch typed := value.(type) {
			case []any:
				if len(typed) > 0 {
					return true
				}
			case map[string]any:
				if len(typed) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// requestHasImages 报告出站请求体里是否带图片块（扫全部消息，含历史）。
//
// 解析失败按「有图片」处理（走 legacy）：宁可少用 Remote 的能力，也不能把用户的
// 图片静默丢掉——丢图的回答看起来像模型没看图，用户无从察觉。
func requestHasImages(prepared []byte) bool {
	if len(prepared) == 0 {
		return false
	}
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return true
	}
	messages, _ := src["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if messageHasImageBlock(message) {
			return true
		}
	}
	return false
}

// messageHasImageBlock 报告一条消息的 content 里是否含图片块。
func messageHasImageBlock(message map[string]any) bool {
	content, ok := message["content"].([]any)
	if !ok {
		return false
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if rendererMediaBlock(block) != nil {
			return true
		}
	}
	return false
}

// chatStreamLegacy 是旧的 llm_utils_chat 出站路径（现为 Remote 的回落）。
//
// 语义与反转前完全一致（含 3004 → Remote 的反向回落），原样保留——它现在是
// 双向冗余的另一半。上游非 2xx 时返回 (nil, status, body, nil) 让上层按既有
// 错误分类处理。
func chatStreamLegacy(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	requestID := newRequestID()
	// 用实时目录把客户端模型名解析成 config_name（含显示名匹配，如
	// "DeepSeek-V4.1-Flash" → "deepseek-v4.1-flash"）；目录不可用时
	// BuildChatBody 内部回落静态别名表。
	//
	// 目录同时决定**出站 function 与该 function 自己的 model_name**：只在
	// code_reviewer / git_ai / multimodal 等专用表里的模型，发到写死的
	// solo_work_lite 必然 `config item is empty for config opt`（2026-10-18 实测）。
	var catalog []CatalogModel
	if models, cerr := FetchCatalog(ctx, a); cerr == nil && len(models) > 0 {
		catalog = models
		if resolved := ResolveFromCatalog(models, modelFromPrepared(prepared)); resolved != "" {
			prepared = withModel(prepared, resolved)
		}
	}
	body, err := BuildChatBodyWithCatalog(prepared, requestID, catalog)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBaseVar+ChatPath, bytes.NewReader(body))
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
	stream := translateStreamWithCredit(resp.Body, modelFromPrepared(prepared),
		preparedTools(prepared), creditForStream(a, requestID))
	// 工具轮次超限（3004）时透明回落到 Remote 会话端点。回落发生在**客户端收到
	// 任何内容之前**才安全——已交付内容再换端点会让用户看到重复/断裂的输出。
	// 触发条件与代价见 remote.go 文件头。
	//
	// 带**当前轮图片**的请求不能回落 Remote：Remote 端点读不到内联图片（见
	// ChatStream 注释的实测），回落等于把图悄悄丢掉，用户会拿到一个「没看图」的
	// 回答而无从察觉。
	//
	// 带**当前轮图片**的请求不能回落 Remote：Remote 端点读不到内联图片（见
	// ChatStream 注释的实测），回落等于把图悄悄丢掉，用户会拿到一个「没看图」的
	// 回答而无从察觉。
	//
	// 出路是「先转写再重发」（2026-10-18 实测修正）：3004 在带图请求上的真实含义
	// 是**该模型没有视觉能力**，不是工具轮次超限。官方客户端也是这么做的——它用
	// multimodal_image_kimi-k2.5 把图转成文字再喂主模型。网关现在把这一步搬进来：
	// 转写图片 → 替换成文本块 → 用同一模型重发。详见 imagetext.go。
	//
	// 判据是 freshInputHasImages（只看本轮新输入），不是 requestHasImages：
	// 用后者会让「历史里贴过一张图」永久禁用该会话的回落，正常提问直接吃 400
	// ——这正是 v3.8.2 引入、v3.8.3 修掉的回归。
	if RemoteEnabled() {
		stream = newRemoteFallbackReader(ctx, a, prepared, clientIP, stream,
			freshInputHasImages(prepared))
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
	// hasImages 本轮新输入是否带图。带图时 3004 走「转写后重发」而不是 Remote
	// （Remote 读不到内联图片，回落等于静默丢图）。
	hasImages bool
}

func newRemoteFallbackReader(ctx context.Context, a *auth.Auth, prepared []byte,
	clientIP string, primary io.ReadCloser, hasImages bool) *remoteFallbackReader {
	return &remoteFallbackReader{ctx: ctx, auth: a, prepared: prepared,
		clientIP: clientIP, primary: primary, hasImages: hasImages}
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
		// 尚未交付实质内容 + 上游明确拒绝 → 换一条路重来一次。
		//
		// 两条路按「本轮是否带图」分派，因为它们解决的是**不同的拒绝**：
		//   - 带图 → 3004 的真实含义是「该模型没有视觉能力」（实测：glm-5.3 带图
		//     502、同模型纯文本 200）。出路是把图转写成文字后用同一模型重发，
		//     见 imagetext.go。不能回落 Remote——那里读不到内联图片。
		//   - 不带图 → 才是真正的工具轮次超限，回落 Remote 绕开结构化工具字段。
		if !r.sawContent && isToolTurnLimitError(err) {
			if r.hasImages {
				transcribed, terr := transcribeImages(r.ctx, r.auth, r.prepared, r.clientIP)
				if terr == nil && transcribed != nil {
					secondary, ferr := r.resend(transcribed)
					if ferr == nil {
						log.Printf("INFO: [trae] 带图请求被 3004 拒绝（模型无视觉能力），已转写图片后用同一模型重发")
						_ = r.primary.Close()
						r.secondary = secondary
						r.buffered = nil
						r.switched = true
						continue
					}
					log.Printf("WARN: [trae] 转写后重发失败：%v（原始错误：%v）", ferr, err)
				} else if terr != nil {
					log.Printf("WARN: [trae] 图片转写失败，无法绕开 3004：%v（原始错误：%v）", terr, err)
				}
				// 转写这条路走不通：如实报错，不再回落 Remote（会静默丢图）。
				r.done = true
				return 0, fmt.Errorf("trae: 该模型不支持图片，且图片转写未能完成（%v）；"+
					"处置：改用支持图片的模型（如 glm-5.3-flash / deepseek-v4.1-flash），或本轮去掉图片", err)
			}
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

// resend 用**改写后的请求体**重打 llm_utils_chat，返回已转译的流。
//
// 与 ChatStreamRemote 的分工：那个换端点，这个换请求形态、端点不变。图片转写
// 之后请求体里已经没有图片块了，所以这里重新算一次目录解析（模型名可能因
// 改写而变），并按新请求体重新取工具白名单。
func (r *remoteFallbackReader) resend(prepared []byte) (io.ReadCloser, error) {
	requestID := newRequestID()
	var catalog []CatalogModel
	if models, cerr := FetchCatalog(r.ctx, r.auth); cerr == nil && len(models) > 0 {
		catalog = models
		if resolved := ResolveFromCatalog(models, modelFromPrepared(prepared)); resolved != "" {
			prepared = withModel(prepared, resolved)
		}
	}
	body, err := BuildChatBodyWithCatalog(prepared, requestID, catalog)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost,
		GatewayBaseVar+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range buildHeaders(r.auth) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if r.clientIP != "" {
		req.Header.Set("X-Forwarded-For", r.clientIP)
	}
	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return translateStreamWithCredit(resp.Body, modelFromPrepared(prepared),
		preparedTools(prepared), creditForStream(r.auth, requestID)), nil
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
// 这是本包的**主路径**（2026-10-16 对齐参考 UPSTREAM_MODE=remote 后反转次序；
// 为什么、以及实测的触发条件与代价，见 remote.go 文件头）。
//
// 与 ChatStream 的差别：这里**不返回上游状态码**给上层做错误分类，而是把两步协议
// 里的失败就地转成 error（建会话失败时上游给的是业务码，不是可分类的 HTTP 状态）。
func ChatStreamRemote(ctx context.Context, a *auth.Auth, prepared []byte, clientIP string) (io.ReadCloser, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body, _, _, err := buildRemoteBody(ctx, a, prepared)
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
	// 链路（三层，顺序不能乱）：
	//   原始 Remote 事件 → firstEventReader（首模型事件超时，看原始事件名）
	//   → translateRemoteEvents（Remote 事件 → llm_utils_chat 形态）
	//   → TranslateStream（llm_utils_chat → OpenAI SSE）。
	// 超时判定必须放在转译**之前**的原始层：转译后的流只有 output 事件，排队期
	// 的 heartbeat 会被转译器吞成 keepalive 注释，届时已无法区分「排队中」与
	// 「模型活动」（对齐参考 stream_events 的 first-event 判定）。
	return &remoteStreamCloser{
		ReadCloser: translateStreamWithCredit(
			translateRemoteEvents(
				newRemoteFirstEventReader(events),
				modelFromPrepared(prepared), session.id),
			modelFromPrepared(prepared),
			preparedTools(prepared),
			remoteCreditSpec(a, session.messageID)),
		auth:    a,
		session: session,
	}, nil
}

// remoteFirstEventReader 包住 Remote 原始事件流，实现首模型事件超时。
//
// 只在**第一个携带正文/终态的事件**（plan_item/message/done/error/token_usage 等）
// 到来之前计时；heartbeat/排队帧不重置计时（对齐参考：排队期计入首事件窗口，
// 超时即失败）。一旦见过模型活动，本包装退化为纯透传。
//
// 判定时序：超时只在**至少尝试读过一次**之后生效——立即可用的模型事件不能被
// 已过期的 deadline 误杀（首读之前不做预检查）。真正的死流（上游一个字节都
// 不发）由外层 ctx 取消兜底；排队期上游会持续发 heartbeat，读写间隙足以触发
// 判定（测试里两次 Read 之间恰好落在判定点上）。
//
// SSE 帧可能被 TCP 分片切开（event: 与名字分属两次 Read），判定用跨 Read 的
// 滚动窗口（最近 512 字节）找 "event: <模型事件名>"，避免帧边界漏判。
type remoteFirstEventReader struct {
	inner         io.Reader
	deadline      time.Time
	timedOut      bool
	sawFirst      bool
	readAttempted bool
	closed        bool
	window        []byte
}

func newRemoteFirstEventReader(inner io.Reader) *remoteFirstEventReader {
	return &remoteFirstEventReader{
		inner:    inner,
		deadline: time.Now().Add(remoteFirstEventTimeout),
	}
}

// remoteModelEventMarkers 首模型事件的 event 名（Remote 原始事件名）。
var remoteModelEventMarkers = []string{
	"plan_item", "message", "assistant_message",
	"response", "text", "output",
	"token_usage", "done", "error",
}

// remoteModelEventPrefix 报告一段 SSE 文本是否含首模型事件的 event 行。
// 认 "event: name" 与 "event:name" 两种拼写（SSE 冒号后空格可选）。
func remoteModelEventPrefix(text string) bool {
	for _, marker := range remoteModelEventMarkers {
		if strings.Contains(text, "event: "+marker) || strings.Contains(text, "event:"+marker) {
			return true
		}
	}
	return false
}

func (r *remoteFirstEventReader) Read(p []byte) (int, error) {
	if r.timedOut {
		return 0, fmt.Errorf("trae remote: 首个模型事件超时（%v）", remoteFirstEventTimeout)
	}
	if r.closed {
		return 0, io.EOF
	}
	if r.readAttempted && !r.sawFirst && time.Now().After(r.deadline) {
		r.timedOut = true
		return 0, fmt.Errorf("trae remote: 首个模型事件超时（%v）", remoteFirstEventTimeout)
	}
	n, err := r.inner.Read(p)
	r.readAttempted = true
	if n > 0 && !r.sawFirst {
		// 滚动窗口：跨分片拼出完整的 event 行再判定。
		r.window = append(r.window, p[:n]...)
		if len(r.window) > 512 {
			r.window = r.window[len(r.window)-512:]
		}
		if remoteModelEventPrefix(string(r.window)) {
			r.sawFirst = true
			r.window = nil
		}
	}
	return n, err
}

func (r *remoteFirstEventReader) Close() error {
	r.closed = true
	if closer, ok := r.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GatewayBaseVar+ChatPath, bytes.NewReader(body))
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

// TranslateStreamWithTools 与 TranslateStream 相同，但声明了调用方提供的工具名。
//
// 为什么需要：Remote 端点不透传 tools 结构，客户端工具只能以 opencode_tool_call
// 文本块表达（prompt.go）。转译层要把这些块解析成 tool_calls，就必须知道**哪些
// 工具名是合法的**——模型偶尔会臆造工具名，交付给客户端会让整轮失败
// （Anthropic 侧拒绝一个未声明的工具调用）。非空列表 = 白名单。
func TranslateStreamWithTools(upstream io.ReadCloser, model string, tools []any) io.ReadCloser {
	return translateStreamWithCredit(upstream, model, tools, nil)
}

// translateStreamWithCredit 同 TranslateStreamWithTools，但挂上流尾查扣费的回调。
//
// credit 为 nil 时不查询（测试、目录缺失、非 trae 路径）。回调在 usage 帧发出前
// 执行一次，把真实 credits 写进 usage.credit——上游事件流本身不带这个字段。
func translateStreamWithCredit(upstream io.ReadCloser, model string, tools []any, credit *creditSpec) io.ReadCloser {
	tr := newTranslator(model)
	if names := declaredToolNames(tools); len(names) > 0 {
		tr.allowedTools = names
	}
	if credit != nil {
		tr.withCredit(credit.scope, credit.turnID, credit.fetcher)
	}
	return newTranslateReader(upstream, tr)
}

// creditSpec 一次查账所需的全部信息。
type creditSpec struct {
	scope   string
	turnID  string
	fetcher creditFetcher
}

// creditForStream 组装 legacy 路径的查账规格（turn id = 我们发的 request_id）。
func creditForStream(a *auth.Auth, requestID string) *creditSpec {
	fetcher := creditFor(a, requestID)
	if fetcher == nil {
		return nil
	}
	return &creditSpec{scope: "llm_utils_chat", turnID: requestID, fetcher: fetcher}
}

// declaredToolNames 收集声明过的工具名（小写，供白名单比对）。
func declaredToolNames(tools []any) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	names := map[string]bool{}
	for _, def := range normalizeToolDefinitions(tools) {
		if name, _ := def["name"].(string); strings.TrimSpace(name) != "" {
			names[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return names
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

// preparedTools 取出站请求体里声明的工具（供文本协议白名单比对）。
//
// 解析失败一律返回 nil（= 不设白名单，全部放行）：白名单是「防臆造」的加固，
// 不该因为一次解析失败就把合法调用也拦掉。
func preparedTools(prepared []byte) []any {
	if len(prepared) == 0 {
		return nil
	}
	var src map[string]any
	if err := json.Unmarshal(prepared, &src); err != nil {
		return nil
	}
	tools, _ := src["tools"].([]any)
	return tools
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

// toolBlockOpeners 是可能出现在正文里的工具块开标签（未闭合检测用）。
//
// 由 tooltext.go 的 textToolTags 派生，**不再单独维护**：此前这里是独立的一份
// toolBlockPairs，与抽取用的 textToolTags 各写一份，实测已经漂移
// （`toolcall` 只抽取不剥离、`bash/read/…` 只剥离不抽取）。单一来源从结构上
// 消灭这类不一致。
func toolBlockOpeners() []string {
	openers := make([]string, 0, len(textToolTags))
	for _, tag := range textToolTags {
		openers = append(openers, "<"+tag)
	}
	return openers
}

// stripToolCallBlocks 移除成对的工具块；未闭合的块由 holdIncompleteToolBlock 压住。
//
// **识别到就剥离，不论能否解析成调用**（保持既有契约，见
// TestVisibleTextHoldsIncompleteToolBlock）。另一种做法是「读不出的块原样保留」
// （magpie 的选择），但那会把原始协议标签漏给用户，而它自己的 #2414 记录过那条路
// 的代价（agent 把原始标签当答案收下、调用彻底丢失）。两条路都有代价：
//   本实现：不泄漏协议标记，代价是读不出的块内容会消失；
//   magpie：内容不丢，代价是原始标签进正文。
// 这里选前者，但**把「内容消失」从静默变成可观测**：translator 统计读不出的块数，
// 收尾时记一行 WARN（见 translator.unparsedBlocks），排障时能看到发生过几次。
func stripToolCallBlocks(text string) string {
	blocks := scanToolBlocks(text)
	if len(blocks) == 0 {
		return text
	}
	var out strings.Builder
	cursor := 0
	for _, block := range blocks {
		out.WriteString(text[cursor:block.start])
		cursor = block.end
	}
	out.WriteString(text[cursor:])
	return out.String()
}

// holdIncompleteToolBlock 截断到第一个「已开始但还没闭合」的工具块之前，
// 避免把半截工具标记当正文发给客户端。
func holdIncompleteToolBlock(text string) string {
	if text == "" {
		return text
	}
	lowered := strings.ToLower(text)
	earliest := len(text)
	for _, opener := range toolBlockOpeners() {
		start := strings.Index(lowered, opener)
		if start < 0 {
			continue
		}
		if strings.Index(lowered[start+len(opener):], "</"+strings.TrimPrefix(opener, "<")+">") < 0 {
			earliest = min(earliest, start)
		}
	}
	// 可能正好停在 "<tool_c" 这种前缀上：压住最后一个 '<' 起的尾巴。
	if last := strings.LastIndex(lowered, "<"); last >= 0 {
		suffix := lowered[last:]
		for _, opener := range toolBlockOpeners() {
			if strings.HasPrefix(opener, suffix) {
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

// absorbTextCalls 把文本协议解析出的调用转成 OpenAI 工具帧。
//
// 与 absorb 分开：absorb 处理上游原生的 tool_calls 片段（首片带 id/name、后续只带
// 参数增量），文本协议解析出的调用则是**一次性完整**的，直接按顺序发完整帧即可
// （客户端按 index 累积，一次发完与分片发完等价）。
func (t *toolAccumulator) absorbTextCalls(calls []textToolCall) []map[string]any {
	var deltas []map[string]any
	for _, call := range calls {
		index := len(t.order)
		state := &toolCallState{id: call.ID, typ: "function", name: call.Name}
		state.arguments.WriteString(call.Arguments)
		state.seenArgs = true
		t.calls[index] = state
		t.order = append(t.order, index)
		deltas = append(deltas, map[string]any{
			"index": index,
			"id":    call.ID,
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
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

	// ── 思考的下发时机（2026-10-16 修「思考被切成很多段」）──────────────────
	//
	// 上游 Remote agent 的一轮回答里会连续发多个 plan_item，**每个都同时带
	// thought 快照与正文快照**。旧实现每收到一个就「先发 reasoning、再发
	// content」，于是下游看到 R,C,R,C,R,C… 交替；Anthropic 侧每遇一次
	// thinking→text→thinking 都要新开一个 content block，Claude Code 便把它
	// 渲染成十几个「Thought for 1s」碎片，中间夹着几个字的正文。
	//
	// 现在改成：正文首次出现之前到达的思考先累积，等正文要发时**一次性**发出
	// （一个 thinking block）；正文开始后到达的思考进 lateReasoning，收尾时
	// 合并成第二个 block 发出（不丢内容，但不再与正文交替）。
	reasoningFlushed bool
	// reasoningDelivered 记录「是否已向客户端交付过任何思考内容」。
	//
	// 供 finish() 的空响应判据使用：只思考不写正文是合法回合，不能报空响应。
	// 三条交付路径（flushReasoning / flushLateReasoning / 直接 emit）都要置位。
	reasoningDelivered bool
	lateReasoning      *textAccumulator
	// reasoningPending/latePending 累积「已从累加器取出但尚未交付」的思考增量。
	// 不能等到 flush 时才调 finalize：add 已经把增量消费掉了。
	reasoningPending strings.Builder
	latePending      strings.Builder
	// pendingText 攒住「已取出但只有空白」的正文增量，等非空白内容出现时一起发
	// （理由见 handleEvent 里正文分支的注释）。
	pendingText strings.Builder

	// textToolSeen 文本协议工具调用的签名集合（跨事件去重，同一调用只交付一次）。
	textToolSeen map[string]bool
	// allowedTools 调用方声明的工具名（小写）。非空时只交付声明过的调用——
	// 模型臆造的工具名交付给客户端会让整轮失败（见 absorbTextToolCalls）。
	allowedTools map[string]bool
	// droppedTools 被过滤掉的未声明调用数（收尾时记一行日志，不静默丢弃）。
	droppedTools int
	// unparsedBlocks 扫到但**读不出调用形态**的工具块数。
	//
	// 这些块会被 stripToolCallBlocks 从正文里删掉：用户看不到内容、也拿不到调用。
	// 计数 + 一份样本在收尾时记日志，让这种「内容消失」可观测——持续非零说明模型
	// 写出的形态超出解析器覆盖（magpie 的 #823 就是这么发现还缺两套格式的）。
	unparsedBlocks int
	unparsedSeen   map[string]bool
	unparsedSample string

	// credit 是流尾查扣费的回调（见 session_usage.go）。
	//
	// 为什么必须查：上游 `token_usage` 事件只有 token 数、**没有 credit 字段**，
	// 于是账本永远拿不到扣费、面板「积分」列恒为「—」。官方客户端另打
	// `get_session_usage` 取真实账单，这里在发 usage 帧之前补上这一步。
	// nil = 不做查询（目录/账号缺失、测试等）。
	credit creditFetcher
	// creditScope 只用于日志前缀（"llm_utils_chat" / "Remote"）。
	creditScope string
	// creditTurnID 查账用的 turn id（legacy = 我们发的 request_id，Remote = message_id）。
	creditTurnID string
}

func newTranslator(model string) *translator {
	return &translator{
		model:         model,
		id:            "chatcmpl-trae-" + newRequestID(),
		text:          &textAccumulator{},
		reasoning:     &textAccumulator{},
		lateReasoning: &textAccumulator{},
		tools:         newToolAccumulator(),
	}
}

// withCredit 挂上流尾查扣费的回调（见 session_usage.go）。
func (t *translator) withCredit(scope, turnID string, fetcher creditFetcher) *translator {
	t.creditScope = scope
	t.creditTurnID = turnID
	t.credit = fetcher
	return t
}

// applyCredit 在 usage 帧发出前补上真实扣费。
//
// 上游事件流不带 credit，而账本与成本账本都靠 usage.credit 判定「本次花了多少」。
// 查不到时**不写**该字段（缺失≠0，见 chatStatsReader.Credit 的注释）。
func (t *translator) applyCredit() {
	if t.credit == nil {
		return
	}
	observed, ok := t.credit()
	if !ok {
		return
	}
	if t.usage == nil {
		t.usage = map[string]any{}
	}
	t.usage["credit"] = observed.Credits
	creditLog(t.creditScope, t.creditTurnID, observed)
}

// absorbReasoning 吸收一段思考（累积快照或增量，由累加器合并）。
//
// 增量由累加器的 add 返回（它同时推进去重状态），这里累积到待发缓冲里，
// 等 flush 时一次性交付——不能等到 flush 再调 finalize，那时 add 已经消费过
// 增量、finalize 只会返回空串（2026-10-16 实测踩到）。
func (t *translator) absorbReasoning(chunk string) {
	if chunk == "" {
		return
	}
	if t.contentCount > 0 {
		// 正文已经开始：这一段的思考改走尾部通道，避免与正文交替成块。
		t.latePending.WriteString(t.lateReasoning.add(chunk))
		return
	}
	t.reasoningPending.WriteString(t.reasoning.add(chunk))
}

// flushReasoning 把正文之前累积的思考一次性发出（只发一次）。
func (t *translator) flushReasoning(out *bytes.Buffer) {
	if t.reasoningFlushed {
		return
	}
	t.reasoningFlushed = true
	// finalize 放行尾部被压住的不完整标记（如恰好停在 "<tool_c" 上）。
	t.reasoningPending.WriteString(t.reasoning.finalize())
	if text := t.reasoningPending.String(); text != "" {
		t.start(out)
		t.reasoningDelivered = true
		t.emit(out, map[string]any{"reasoning_content": text}, nil)
	}
	// 尾部通道**继承**正文前的去重状态。
	//
	// 上游发的是**累计快照**：正文之后到达的 thought 通常是「正文前那段 + 新增」
	// 的完整前缀扩展。若尾部通道从空状态起步，那段前缀会被当成全新文本整段再交付
	// 一次——生产实测的表现就是 thinking 首尾相接重复两遍（同一段话出现两次）。
	// 继承 raw/visible 后，add() 的前缀匹配会正确算出「只交付新增部分」。
	// 新思考（与前面无关）走 default 分支拼接，不受影响。
	t.lateReasoning.raw = t.reasoning.raw
	t.lateReasoning.visible = t.reasoning.visible
}

// flushLateReasoning 收尾时发出「正文开始后」到达的思考（合并成一个 block）。
func (t *translator) flushLateReasoning(out *bytes.Buffer) {
	t.latePending.WriteString(t.lateReasoning.finalize())
	if text := t.latePending.String(); text != "" {
		t.start(out)
		t.reasoningDelivered = true
		t.emit(out, map[string]any{"reasoning_content": text}, nil)
	}
}

// absorbTextToolCalls 从累积原文与思考里抽取文本协议的工具调用并交付。
//
// 必须在「累积原文」上扫描而不是每次的增量：块会跨事件分片，任何单片里都看不到
// 闭合标签。签名去重保证同一调用只交付一次。详见 tooltext.go 文件头。
func (t *translator) absorbTextToolCalls(out *bytes.Buffer) {
	if t.textToolSeen == nil {
		t.textToolSeen = map[string]bool{}
	}
	var found []textToolCall
	for _, raw := range []string{t.text.raw, t.reasoning.raw, t.lateReasoning.raw} {
		found = append(found, extractTextToolCalls(raw, t.textToolSeen)...)
		// 观测：扫到但读不出调用形态的块。这些块会被 stripToolCallBlocks 从正文里
		// 删掉，用户看不到内容、也拿不到调用——如果数量持续不为零，说明模型写出的
		// 形态超出解析器覆盖范围（magpie 的 #823 就是这么长出两套额外格式的）。
		// 用 seen 集合去重，避免每个分片重复计数同一块。
		if t.unparsedSeen == nil {
			t.unparsedSeen = map[string]bool{}
		}
		for _, block := range scanToolBlocks(raw) {
			if _, ok := parseTextToolCall(block.attrs, block.body); ok {
				continue
			}
			key := block.tag + "\x00" + block.body
			if t.unparsedSeen[key] {
				continue
			}
			t.unparsedSeen[key] = true
			t.unparsedBlocks++
			// 只留开头一段样本，够识别形态即可（正文可能很长）。
			if t.unparsedSample == "" {
				t.unparsedSample = "<" + block.tag + ">" + truncate(strings.TrimSpace(block.body), 160)
			}
		}
	}
	if len(found) == 0 {
		return
	}
	// 过滤模型臆造的工具名：未声明的调用交付给客户端会整轮失败（Anthropic 侧
	// 会以「上游要求调用一个不存在的工具」拒绝整条消息）。
	kept := found[:0]
	for _, call := range found {
		if len(t.allowedTools) > 0 && !t.allowedTools[strings.ToLower(call.Name)] {
			t.droppedTools++
			continue
		}
		kept = append(kept, call)
	}
	for _, delta := range t.tools.absorbTextCalls(kept) {
		t.start(out)
		t.emit(out, map[string]any{"tool_calls": []any{delta}}, nil)
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

	// 思考先累积（不与正文交替——见 translator.reasoningFlushed 注释）。
	for _, chunk := range []string{reasoning, embeddedReasoning} {
		t.absorbReasoning(chunk)
	}
	if addition := t.text.add(visibleResponse); addition != "" {
		// 正文要发了：先把之前累积的思考一次性放出去，再开正文块。
		//
		// 但**纯空白**的正文增量不算「正文开始」：实测上游每个 plan_item 的
		// content 字段常常只有一个换行，于是思考分片之间夹着 C("\n")，Anthropic
		// 侧就反复 thinking→text→thinking 开关块，Claude Code 渲染成十几个
		// 「Thought for 1s」（用户报障的形态：每段思考都被切成独立条目）。
		// 空白先攒着，等真出现非空白内容时连同它一起发——这样连续思考就落在
		// 同一个 thinking 块里。
		t.pendingText.WriteString(addition)
		if strings.TrimSpace(t.pendingText.String()) != "" {
			t.flushReasoning(out)
			t.start(out)
			t.contentCount++
			t.emit(out, map[string]any{"content": t.pendingText.String()}, nil)
			t.pendingText.Reset()
		}
	}
	// 文本协议的工具调用：模型把 opencode_tool_call 写在正文或思考里，
	// 这里抽取成标准 tool_calls 帧（旧版只 strip 不抽取，调用凭空消失）。
	t.absorbTextToolCalls(out)
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
		t.pendingText.WriteString(addition)
	}
	if t.pendingText.Len() > 0 {
		t.flushReasoning(out)
		t.start(out)
		t.contentCount++
		t.emit(out, map[string]any{"content": t.pendingText.String()}, nil)
		t.pendingText.Reset()
	}
	// 正文开始后到达的思考（以及与正文同时收尾的那批）：合并成尾部一个块。
	t.flushLateReasoning(out)
	// 全文已到齐，做最后一次抽取：分片恰好停在收尾处时前面的扫描看不到闭合标签。
	t.absorbTextToolCalls(out)
	// 正文之前完全没发过思考（例如纯工具轮次的思考只在 reasoning.raw 里）：
	// 补一次，保证思考不丢。
	t.flushReasoning(out)
	if t.droppedTools > 0 {
		log.Printf("WARN: [trae] 丢弃 %d 个未声明工具的文本调用（模型臆造的工具名）", t.droppedTools)
	}
	// 读不出的块：内容会被剥离掉，所以必须留痕，否则「模型发了工具块但什么都没发生」
	// 只能靠猜。样本用于识别模型实际写出的形态。
	if t.unparsedBlocks > 0 {
		log.Printf("WARN: [trae] %d 个工具块无法解析成调用，已从正文剥离（内容丢失）；样本=%s",
			t.unparsedBlocks, t.unparsedSample)
	}
	// 「上游什么都没返回」的判据必须把**思考**也算作已交付内容。
	//
	// 生产事故（2026-10-16 用户截图）：模型只输出思考（该轮 16,291 字符）就收尾，
	// 正文一个字都没写，客户端看到的却是占位文案
	// 「(trae upstream returned an empty response)」——思考明明已经交付了。
	// 对 Anthropic 客户端来说，只带 thinking block 的回合完全合法（想完就停），
	// 网关不该替它断言上游没返回东西。只有「正文、思考、工具调用三者全空」
	// 才是真的空响应。
	if t.contentCount == 0 && !t.tools.hasCalls() && !t.reasoningDelivered {
		t.start(out)
		t.emit(out, map[string]any{"content": "(trae upstream returned an empty response)"}, nil)
	}
	reason := finalFinishReason(t.finishReason, t.tools.hasCalls())
	t.emit(out, map[string]any{}, reason)
	// 补真实扣费：上游 token_usage 事件没有 credit 字段（见 session_usage.go）。
	// 必须在 usage 帧之前做——账本读的就是这一帧的 usage.credit。
	t.applyCredit()
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
	// closedByCaller 调用方主动 Close（回落切端点时关掉 primary 走这里）。
	//
	// 为什么需要：run 在「一帧都没收到」时会把 err 设成 no data events。回落路径
	// 主动关闭 primary 时，这个判定会把「正常关闭」误报成上游故障，日志里出现
	// 「Trae upstream returned no data events（已收 0 字节）」——它看起来像 Remote
	// 失败，实际是 legacy 那条流被我们关掉了（2026-10-18 排查被这条误导过）。
	closedByCaller atomic.Bool
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
		// 调用方主动关闭（回落切端点）导致的「零帧」不是上游故障：此时 primary
		// 的 body 被 Close，读取立即返回，看起来与「上游建立连接却不发数据」完全
		// 一样。只有真正没被主动关闭的空流才值得报错。
		if r.closedByCaller.Load() {
			return
		}
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
	r.closedByCaller.Store(true)
	_ = r.pr.Close()
	<-r.done
	return nil
}
