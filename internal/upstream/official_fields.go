// official_fields.go 补齐官方客户端每轮必发的 chat 顶层字段。
//
// ═══ 为什么需要（2026-11-04）═══
//
// 6 份官方抓包（5.8.3 / 5.8.4 / 5.9.3，CN + AI 两域）逐份解析，顶层键集合逐字相同：
//
//	model, messages, tools, temperature, max_tokens, stream, stream_options,
//	reasoning_effort, verbosity, extra_vars, reasoning_summary
//
// 而网关出站只有：
//
//	max_tokens, messages, model, prompt_cache_key, reasoning_effort,
//	store, stream, stream_options, thinking, tools
//
// 少了 temperature / verbosity / reasoning_summary / extra_vars 四个，
// 多了 prompt_cache_key / thinking（官方全客户端 0 处存在这两个字段名）。
//
// 这不是 11128 的直接触发点（单变量回放实测：删掉多发的三个字段仍被拒，
// 见 workspace_root_test.go 的排查记录），但字段集合本身就是可判定的客户端指纹——
// 补齐能让出站形态与官方一致，减少被按「非官方」整体打标的概率。
//
// 反向证据（不该擅自删的）：`thinking` 是 DeepSeek 思维链开关，删掉会改变模型行为；
// `prompt_cache_key` 带来 ~17× 的费用下降。两者都是功能所需，本轮只补不删。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"workbuddy2api/internal/jsonutil"
)

// 官方取值（6/6 抓包一致，见文件头）。
const (
	// officialTemperature 官方恒为 1。客户端不给 temperature 时才补，
	// 调用方显式指定（含 0）一律尊重——0 是合法且有意义的值。
	officialTemperature = 1.0
	// officialVerbosity 官方恒为 "high"。
	officialVerbosity = "high"
	// officialReasoningSummary 官方恒为 "auto"。
	officialReasoningSummary = "auto"
)

// ApplyOfficialChatFields 补齐官方客户端每轮必发、而调用方没给的顶层字段。
//
// 只在缺字段时补，已有值原样保留（客户端自知想要什么）。
// body 不可解析时原样返回，与 prepareChatBody 的失败语义一致。
//
// modelName 用于 extra_vars.growthEvent 里的 requestModelId / requestModelName，
// 应与出站 body 的 model 字段一致（调用方传解析后的裸模型名）。
// inputLength 是官方 growthEvent 里的输入长度估算，0 表示未知（此时不填该键，
// 而不是谎报 0）。
func ApplyOfficialChatFields(body []byte, modelName string, inputLength int) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil || obj == nil {
		return body
	}
	changed := false

	// temperature：官方恒 1。只判「键不存在」，不判值——显式 0 必须保留。
	if _, ok := obj["temperature"]; !ok {
		obj["temperature"] = officialTemperature
		changed = true
	}
	if _, ok := obj["verbosity"]; !ok {
		obj["verbosity"] = officialVerbosity
		changed = true
	}
	if _, ok := obj["reasoning_summary"]; !ok {
		obj["reasoning_summary"] = officialReasoningSummary
		changed = true
	}
	// extra_vars：官方每轮必发。形状见 buildGrowthEvent。
	if _, ok := obj["extra_vars"]; !ok {
		model, _ := obj["model"].(string)
		if model == "" {
			model = modelName
		}
		obj["extra_vars"] = map[string]any{
			"growthEvent": buildGrowthEvent(model, inputLength),
			"initiator":   "user",
		}
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildGrowthEvent 构造官方 `extra_vars.growthEvent` 的内容。
//
// 官方原值形如（抓包 20261029-103217.049-033）：
//
//	[{"eventCode":"chat_request_send","id":"1a9795d6-9061-4654-8259-19b774495fef",
//	  "extra":{"inputLength":111144,"requestModelId":"deepseek-v4.1-flash",
//	           "requestModelName":"DeepSeek-V4.1-Flash","mode":"craft","command":""}}]
//
// 注意它是**字符串**而不是 JSON 数组——官方把数组序列化后当字符串塞进 growthEvent。
// 照抄这个形态：多一层序列化，否则上游按字符串解析时拿不到内容。
//
// requestModelName 用「DeepSeek-V4.1-Flash」这种展示名：官方在不同版本里大小写
// 不一致（033 是 DeepSeek-V4.1-Flash、039 是 Deepseek-V4.1-Flash），说明它只作展示，
// 这里按 model id 推导一个可读形式即可。
func buildGrowthEvent(model string, inputLength int) string {
	extra := map[string]any{
		"requestModelId":   model,
		"requestModelName": displayModelName(model),
		"mode":             "craft",
		"command":          "",
	}
	if inputLength > 0 {
		extra["inputLength"] = inputLength
	}
	payload := []any{map[string]any{
		"eventCode": "chat_request_send",
		"id":        randomUUIDv4(),
		"extra":     extra,
	}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// displayModelName 把模型 id 转成官方 growthEvent 里的展示名。
//
// "deepseek-v4.1-flash" -> "DeepSeek-V4.1-Flash"
//
// 首段遵循已知品牌大小写（DeepSeek / GLM / HY），未知品牌才退化为首字母大写；
// 其余段若以字母开头则首字母大写（v4.1 -> V4.1、flash -> Flash）。
// 纯数字段原样保留。
//
// 为什么需要品牌表：`deepseek` 首字母大写得 "Deepseek"，而官方抓包里是
// "DeepSeek"（033）/"Deepseek"（039）两种混用——它只是展示字段，但按品牌表
// 写更接近真实形态，也避免拼错已知品牌。
func displayModelName(model string) string {
	if model == "" {
		return ""
	}
	parts := strings.Split(model, "-")
	// 首段品牌大小写：先精确匹配，再按前缀匹配（hy3 -> HY3、deepseek-v4.1 已由
	// 精确分支处理）。按前缀从长到短试，避免 "h" 这类短键先命中。
	if brand, ok := modelBrandCase[strings.ToLower(parts[0])]; ok {
		parts[0] = brand
	} else if brand, rest, ok := prefixBrandCase(parts[0]); ok {
		parts[0] = brand + rest
	} else if parts[0] != "" {
		parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		// 后续段：字母开头才大写首字母（v4.1 -> V4.1，数字开头原样）
		if c := part[0]; (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "-")
}

// modelBrandCase 已知模型系列的官方大小写写法。
var modelBrandCase = map[string]string{
	"deepseek": "DeepSeek",
	"glm":      "GLM",
	"hy":       "HY",
	"kimi":     "Kimi",
	"minimax":  "MiniMax",
	"mimo":     "MiMo",
	"step":     "Step",
}

// prefixBrandCase 处理「品牌名后直接跟型号」的写法（hy3 -> HY3、kimi2 -> Kimi2）。
// 按品牌名长度降序尝试，避免短品牌先命中；无匹配返回 ok=false。
func prefixBrandCase(segment string) (brand, rest string, ok bool) {
	lowered := strings.ToLower(segment)
	best := ""
	for key := range modelBrandCase {
		if strings.HasPrefix(lowered, key) && len(key) > len(best) && len(lowered) > len(key) {
			// 品牌名后面必须是数字或非字母，否则 "hymnal" 会被当成 HY 品牌。
			next := lowered[len(key)]
			if (next >= '0' && next <= '9') || next == '.' {
				best = key
			}
		}
	}
	if best == "" {
		return "", "", false
	}
	return modelBrandCase[best], segment[len(best):], true
}

// randomUUIDv4 生成带横线的 UUIDv4（官方 growthEvent 的 id 形态）。
// 失败时回落时间戳派生的 hex，保证字段非空——这个 id 只作展示/去重，
// 不承载任何鉴权语义。
func randomUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况：用纳秒时间戳填满，形态仍是 32 hex。
		ns := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(ns >> (8 * i))
			b[8+i] = byte(ns >> (8 * (7 - i)))
		}
	}
	b[6] = (b[6] & 0x0F) | 0x40 // 版本 4
	b[8] = (b[8] & 0x3F) | 0x80 // 变体 RFC 4122
	raw := hex.EncodeToString(b[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
}
