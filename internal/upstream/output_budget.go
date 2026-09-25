// ═══ 更新日志 ═══
// 2026-09-26：客户端未声明输出预算时按模型目录 maxOutputTokens 补齐（与官方客户端一致）；
//
//	上游按「输入+输出预算」判超限时，只收缩网关自选的预算后同号重发，不改历史；
//	窗口边界处无字段的 11133 先缩预算重试，仍被拒则按上下文超限返回，让客户端压缩；
//	长会话按上一轮真实输入主动收缩预算，并学习上游实报窗口，免去每轮先被拒一次。
//
// output_budget.go 管理发往上游的输出预算（max_tokens）。
//
// 背景（2026-09-25 线上合成对照，global:deepseek-v4.1-flash）：上游判定上下文时计算的是
// 「输入 token + max_tokens」，窗口 1048576；请求不带 max_tokens 时上游默认预留 384000。
// 于是不带预算的客户端（Codex、多数 OpenAI 兼容客户端）输入超过 664576 就被拒——远低于
// 模型标称的 1M 窗口。边界附近上游预检放行、模型提供方再拒时，返回的是无字段的
// 11133 model_param_invalid，客户端既看不出是超限，也不会触发压缩。
//
// 官方 WorkBuddy 客户端始终发送 max_tokens = 目录 maxOutputTokens，并在
// 「输入 + max_tokens ≥ 窗口」时先压缩。网关在客户端未声明预算时采用同一取值。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"workbuddy2api/internal/jsonutil"
)

const (
	// minFittedOutputTokens 收缩后的输出预算下限。推理模型的思考也计入 max_tokens，
	// 预算再小就可能只够思考、没有正文；低于下限不再重发，原样返回超限让客户端压缩。
	minFittedOutputTokens int64 = 32768
	// fitMarginTokens 按上游报告的精确计数重算预算时额外留出的余量。
	fitMarginTokens int64 = 256
	// maxBudgetRetries 单个请求内因输出预算重发的上限（缩预算一次 + 精确重算一次）。
	maxBudgetRetries = 2
	// nearWindowRatio 估算输入+输出预算达到窗口的这个比例，才把无字段 11133 归因到窗口边界。
	nearWindowRatio = 0.75
)

// modelLimits 模型目录里与请求预算相关的两项上限（0 = 未知）。
type modelLimits struct {
	maxOutput int64
	context   int64
}

// outputBudget 记录一次出站请求实际携带的输出预算。
type outputBudget struct {
	model         string
	tokens        int64 // 出站 max_tokens（0 = 未携带）
	gatewayChosen bool  // 客户端未声明、由网关按目录补齐；只有这种预算允许网关收缩
	context       int64 // 目录窗口（0 = 未知），用于判断 11133 是否发生在窗口边界
}

// storeModelLimits 按 realm 写入模型目录上限。空目录不写，避免一次异常探测清掉已知上限。
func (c *Client) storeModelLimits(realm string, infos []ModelInfo) {
	if len(infos) == 0 {
		return
	}
	bucket := make(map[string]modelLimits, len(infos))
	for _, mi := range infos {
		if mi.ID == "" || (mi.MaxTokens <= 0 && mi.ContextWindow <= 0) {
			continue
		}
		bucket[mi.ID] = modelLimits{maxOutput: mi.MaxTokens, context: mi.ContextWindow}
	}
	c.limitsMu.Lock()
	defer c.limitsMu.Unlock()
	if c.limits == nil {
		c.limits = make(map[string]map[string]modelLimits)
	}
	c.limits[realmKey(realm)] = bucket
}

// ModelCatalogLoaded 报告该 realm 是否已加载过模型目录（决定能否按目录补齐输出预算）。
func (c *Client) ModelCatalogLoaded(realm string) bool {
	c.limitsMu.RLock()
	defer c.limitsMu.RUnlock()
	_, ok := c.limits[realmKey(realm)]
	return ok
}

// limitsSnapshot 返回该 realm 的目录上限（只读共享；写入时整桶替换，不原地修改）。
func (c *Client) limitsSnapshot(realm string) map[string]modelLimits {
	c.limitsMu.RLock()
	defer c.limitsMu.RUnlock()
	return c.limits[realmKey(realm)]
}

// applyOutputBudget 在客户端未声明正整数 max_tokens 时补上目录 maxOutputTokens。
// 客户端显式给出的值（含 0、负数等畸形值）一律不改，交给既有校验与上游处理。
func applyOutputBudget(obj map[string]any, limits map[string]modelLimits) outputBudget {
	model, _ := obj["model"].(string)
	lim := limits[model]
	budget := outputBudget{model: model, context: lim.context}
	if value, present := obj["max_tokens"]; present && value != nil {
		if number, ok := value.(json.Number); ok {
			if count, err := number.Int64(); err == nil && count > 0 {
				budget.tokens = count
			}
		}
		return budget
	}
	if lim.maxOutput <= 0 {
		return budget
	}
	obj["max_tokens"] = json.Number(strconv.FormatInt(lim.maxOutput, 10))
	budget.tokens, budget.gatewayChosen = lim.maxOutput, true
	return budget
}

// withOutputBudget 返回把 max_tokens 改为 tokens 的请求体副本；其余内容保持不变。
func withOutputBudget(body []byte, tokens int64) ([]byte, bool) {
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return nil, false
	}
	obj["max_tokens"] = json.Number(strconv.FormatInt(tokens, 10))
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// promptTooLongCounts 解析 "prompt is too long: 1084041 tokens > 1048576 maximum"。
// 上游的第一个数是「输入 + max_tokens」，第二个数是窗口。
var promptTooLongCounts = regexp.MustCompile(`(\d+)\s*tokens\s*>\s*(\d+)\s*maximum`)

// fittedOutputBudget 根据上游报告的精确计数，算出让本请求放进窗口的输出预算。
// 仅对网关自选的预算生效；客户端显式预算、计数缺失或剩余空间低于下限时返回 false。
func fittedOutputBudget(budget outputBudget, detail string) (int64, bool) {
	if !budget.gatewayChosen || budget.tokens <= 0 {
		return 0, false
	}
	match := promptTooLongCounts.FindStringSubmatch(detail)
	if match == nil {
		return 0, false
	}
	total, err1 := strconv.ParseInt(match[1], 10, 64)
	window, err2 := strconv.ParseInt(match[2], 10, 64)
	if err1 != nil || err2 != nil || total <= window {
		return 0, false
	}
	prompt := total - budget.tokens
	if prompt <= 0 {
		return 0, false
	}
	fitted := window - prompt - fitMarginTokens
	if fitted < minFittedOutputTokens || fitted >= budget.tokens {
		return 0, false
	}
	return fitted, true
}

// shrunkOutputBudget 为窗口边界处的 11133 选一个更小的网关预算（无精确计数时按 1/4 收缩）。
func shrunkOutputBudget(budget outputBudget) (int64, bool) {
	if !budget.gatewayChosen {
		return 0, false
	}
	next := budget.tokens / 4
	if next < minFittedOutputTokens {
		next = minFittedOutputTokens
	}
	if next >= budget.tokens {
		return 0, false
	}
	return next, true
}

// modelParamRejected 识别模型提供方拒绝参数且未指明字段的 11133（model_param_invalid）。
// 指明了具体字段的 11133 是真实参数问题，不归因到窗口。
func modelParamRejected(raw []byte) (requestID string, ok bool) {
	var failure map[string]any
	if jsonutil.Decode(raw, &failure) != nil || failure == nil {
		return "", false
	}
	if code, isNumber := failure["code"].(json.Number); !isNumber || code.String() != "11133" {
		return "", false
	}
	extended, _ := failure["extError"].(map[string]any)
	if kind, _ := extended["code"].(string); kind != "model_param_invalid" {
		return "", false
	}
	if param, _ := extended["param"].(string); param != "" {
		return "", false
	}
	requestID, _ = failure["requestId"].(string)
	return requestID, true
}

// nearContextWindow 判断请求是否已贴近窗口：估算输入 + 输出预算 ≥ 窗口的 nearWindowRatio。
// 窗口或预算未知时返回 false（保守：不把参数错误改判成超限）。
func nearContextWindow(body []byte, budget outputBudget) bool {
	if budget.context <= 0 || budget.tokens <= 0 {
		return false
	}
	estimate := estimatePromptTokens(body)
	return float64(estimate+budget.tokens) >= nearWindowRatio*float64(budget.context)
}

// estimatePromptTokens 粗估请求体的文本 token 数，口径与官方客户端 estimateTokensRough 一致：
// ceil(中日韩字符/1.8) + ceil(其他字符/4)。base64 图片数据不计入（图片由视觉编码器单独计费）。
// 只用于判断是否贴近窗口，不参与计量。
func estimatePromptTokens(body []byte) int64 {
	var cjk, other int64
	dataURI := []byte("data:")
	for i := 0; i < len(body); {
		if body[i] == 'd' && bytes.HasPrefix(body[i:], dataURI) {
			if end := bytes.IndexByte(body[i:], '"'); end > 0 && bytes.Contains(body[i:i+end], []byte(";base64,")) {
				i += end
				continue
			}
		}
		r, size := utf8.DecodeRune(body[i:])
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			cjk++
		} else {
			other++
		}
		i += size
	}
	return (cjk*10+17)/18 + (other+3)/4
}

// boundaryContextFailure 把窗口边界处的 11133 表述为上下文超限（保留上游原始错误）。
// 返回形态与上游 11115 一致，下游分类、各协议的超限终态与客户端压缩逻辑无需特判。
func boundaryContextFailure(raw []byte, budget outputBudget, requestID string) []byte {
	message := fmt.Sprintf("prompt is too long: the input plus a %d-token output budget reaches the model context window "+
		"(upstream answered 11133 model_param_invalid at the window boundary, requestId=%s); compact the conversation and retry",
		budget.tokens, requestID)
	envelope := map[string]any{
		"code":     11115,
		"msg":      message,
		"extError": map[string]any{"code": "context_length_exceeded"},
	}
	var original any
	if jsonutil.Decode(raw, &original) == nil {
		envelope["upstream"] = original
	} else {
		envelope["upstream"] = string(raw)
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return raw
	}
	return out
}

// ── 主动预算：按同一会话上一轮的真实输入，发送前就把网关预算收到放得下 ──
//
// 被动收缩需要先被上游拒一次（实测预检拒绝约 3.5–5 秒）。长会话进入窗口尾段后每一轮都会
// 撞一次墙，所以网关记住每个会话最近一次上游报告的 prompt_tokens，以及当时请求正文的
// 估算量；下一轮按正文增长比例推算本轮输入，直接发送合适的 max_tokens。推算偏小时仍由
// 被动精确重算兜底，偏大只是少给一点输出预算（不低于 minFittedOutputTokens）。

const (
	promptSampleTTL = 12 * time.Hour
	promptSampleMax = 4096
	// proactiveMinShare 上一轮输入低于窗口的这个比例时不记录、不推算（短会话零开销）。
	proactiveMinShare = 0.25
	// proactiveSafety 推算输入的放大系数，吸收同会话内内容构成变化带来的估算误差。
	proactiveSafety = 1.03
)

type promptSample struct {
	prompt  int64 // 上游报告的输入 token（不含输出预算）
	measure int64 // 当时请求正文的 estimatePromptTokens
	at      time.Time
}

type budgetSessionContextKey struct{}

// WithBudgetSession 把会话键交给出站层，用于按会话推算输出预算。空键不附加。
func WithBudgetSession(ctx context.Context, session string) context.Context {
	if session == "" {
		return ctx
	}
	return context.WithValue(ctx, budgetSessionContextKey{}, session)
}

func budgetSession(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	session, _ := ctx.Value(budgetSessionContextKey{}).(string)
	return session
}

func sampleKey(session, realm, model string) string {
	return realmKey(realm) + "\x00" + model + "\x00" + session
}

// learnWindow 记录上游在超限报文里给出的真实窗口（如 1048576），优先于目录标称值（如 1000000）。
func (c *Client) learnWindow(realm, model string, detail string) {
	match := promptTooLongCounts.FindStringSubmatch(detail)
	if match == nil || model == "" {
		return
	}
	window, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || window <= 0 {
		return
	}
	c.limitsMu.Lock()
	defer c.limitsMu.Unlock()
	if c.windows == nil {
		c.windows = make(map[string]int64)
	}
	c.windows[realmKey(realm)+"\x00"+model] = window
}

// windowFor 返回模型窗口：上游实报优先，其次目录值；都未知返回 0。
func (c *Client) windowFor(realm, model string) int64 {
	c.limitsMu.RLock()
	defer c.limitsMu.RUnlock()
	if window := c.windows[realmKey(realm)+"\x00"+model]; window > 0 {
		return window
	}
	return c.limits[realmKey(realm)][model].context
}

// RecordPromptSample 记录会话最近一次成功请求的真实输入。只保留长会话的样本；
// 输入回落到阈值以下（例如客户端刚压缩过）时删除旧样本，避免拿压缩前的数字推算。
func (c *Client) RecordPromptSample(session, realm, model string, promptTokens int64, body []byte) {
	if session == "" || model == "" || promptTokens <= 0 {
		return
	}
	window := c.windowFor(realm, model)
	key := sampleKey(session, realm, model)
	if window <= 0 || float64(promptTokens) < proactiveMinShare*float64(window) || promptTokens > window {
		c.samplesMu.Lock()
		delete(c.samples, key)
		c.samplesMu.Unlock()
		return
	}
	measure := estimatePromptTokens(body)
	if measure <= 0 {
		return
	}
	now := time.Now()
	c.samplesMu.Lock()
	defer c.samplesMu.Unlock()
	if c.samples == nil {
		c.samples = make(map[string]promptSample)
	}
	if len(c.samples) >= promptSampleMax {
		oldestKey, oldest := "", now
		for k, v := range c.samples {
			if now.Sub(v.at) > promptSampleTTL {
				delete(c.samples, k)
				continue
			}
			if v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		if len(c.samples) >= promptSampleMax && oldestKey != "" {
			delete(c.samples, oldestKey)
		}
	}
	c.samples[key] = promptSample{prompt: promptTokens, measure: measure, at: now}
}

// proactiveOutputBudget 按会话样本推算本轮输入，返回放得进窗口的网关预算。
// 只收缩、不放大；推算结果不低于 minFittedOutputTokens；样本缺失或过期时返回 false。
func (c *Client) proactiveOutputBudget(session, realm string, budget outputBudget, body []byte) (int64, bool) {
	if session == "" || !budget.gatewayChosen || budget.tokens <= minFittedOutputTokens {
		return 0, false
	}
	key := sampleKey(session, realm, budget.model)
	c.samplesMu.Lock()
	sample, ok := c.samples[key]
	if ok && time.Since(sample.at) > promptSampleTTL {
		delete(c.samples, key)
		ok = false
	}
	c.samplesMu.Unlock()
	if !ok || sample.measure <= 0 {
		return 0, false
	}
	window := c.windowFor(realm, budget.model)
	if window <= 0 {
		return 0, false
	}
	measure := estimatePromptTokens(body)
	if measure <= 0 {
		return 0, false
	}
	estimate := int64(float64(sample.prompt) * float64(measure) / float64(sample.measure) * proactiveSafety)
	fitted := window - estimate - fitMarginTokens
	if fitted >= budget.tokens {
		return 0, false
	}
	if fitted < minFittedOutputTokens {
		fitted = minFittedOutputTokens
	}
	return fitted, true
}
