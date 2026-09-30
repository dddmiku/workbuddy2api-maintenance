// ═══ 更新日志 ═══
// 2026-09-25：校验 Gemini 常用生成参数及结构化输出，推理预算不伪装成上游保证。
// 2026-09-25：规范官方 SDK 的大写推理枚举与 NF 小写值，仍只支持 low/medium/high。
package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

func geminiGenerationConfig(value any, chat map[string]any) (bool, error) {
	if value == nil {
		return false, nil
	}
	config, err := requestValidationObject(value, "generationConfig")
	if err != nil {
		return false, err
	}
	if err := geminiOnlyFields(config, "generationConfig", "candidateCount", "maxOutputTokens", "temperature", "topP", "topK", "stopSequences", "seed", "presencePenalty", "frequencyPenalty", "thinkingConfig", "responseMimeType", "responseSchema", "responseJsonSchema"); err != nil {
		return false, err
	}
	if err := requestValidationOptionalNumbers(config, "generationConfig", true, "candidateCount", "maxOutputTokens", "topK", "seed"); err != nil {
		return false, err
	}
	for _, pair := range [][2]string{{"candidateCount", "n"}, {"maxOutputTokens", "max_tokens"}, {"topK", "top_k"}, {"seed", "seed"}} {
		if raw := config[pair[0]]; raw != nil {
			n, err := raw.(json.Number).Int64()
			if err != nil || (pair[0] == "candidateCount" && n != 1) || (pair[0] == "maxOutputTokens" && n <= 0) || (pair[0] == "topK" && n < 0) {
				return false, fmt.Errorf("generationConfig.%s is outside the supported range; candidateCount must be 1", pair[0])
			}
			chat[pair[1]] = raw
		}
	}
	if err := requestValidationOptionalNumbers(config, "generationConfig", false, "temperature", "topP", "presencePenalty", "frequencyPenalty"); err != nil {
		return false, err
	}
	for _, pair := range [][2]string{{"temperature", "temperature"}, {"topP", "top_p"}, {"presencePenalty", "presence_penalty"}, {"frequencyPenalty", "frequency_penalty"}} {
		if raw := config[pair[0]]; raw != nil {
			n, _ := raw.(json.Number).Float64()
			low, high := 0.0, 2.0
			if pair[0] == "topP" {
				high = 1
			} else if pair[0] == "presencePenalty" || pair[0] == "frequencyPenalty" {
				low = -2
			}
			if n < low || n > high {
				return false, fmt.Errorf("generationConfig.%s is outside the supported range", pair[0])
			}
			chat[pair[1]] = raw
		}
	}
	if stop := config["stopSequences"]; stop != nil {
		if err := requestValidationStringArray(stop, "generationConfig.stopSequences"); err != nil {
			return false, err
		}
		chat["stop"] = stop
	}
	if err := requestValidationOptionalStrings(config, "generationConfig", "responseMimeType"); err != nil {
		return false, err
	}
	schema := config["responseSchema"]
	if schema != nil && config["responseJsonSchema"] != nil {
		return false, fmt.Errorf("generationConfig cannot combine responseSchema and responseJsonSchema")
	}
	if config["responseJsonSchema"] != nil {
		schema = config["responseJsonSchema"]
	}
	switch stringField(config, "responseMimeType") {
	case "", "text/plain":
		if schema != nil {
			return false, fmt.Errorf("generationConfig response schema requires responseMimeType application/json")
		}
	case "application/json":
		if schema == nil {
			chat["response_format"] = map[string]any{"type": "json_object"}
		} else {
			converted, err := geminiSchema(schema, "generationConfig.responseSchema")
			if err != nil {
				return false, err
			}
			chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": converted, "strict": true}}
		}
	default:
		return false, fmt.Errorf("generationConfig.responseMimeType supports only text/plain or application/json")
	}
	if config["thinkingConfig"] == nil {
		return false, nil
	}
	thinking, err := requestValidationObject(config["thinkingConfig"], "generationConfig.thinkingConfig")
	if err != nil {
		return false, err
	}
	if err := geminiOnlyFields(thinking, "generationConfig.thinkingConfig", "includeThoughts", "thinkingLevel", "thinkingBudget"); err != nil {
		return false, err
	}
	if err := requestValidationOptionalBools(thinking, "generationConfig.thinkingConfig", "includeThoughts"); err != nil {
		return false, err
	}
	if err := requestValidationOptionalStrings(thinking, "generationConfig.thinkingConfig", "thinkingLevel"); err != nil {
		return false, err
	}
	// thinkingBudget 只校验「是整数」，取值范围交给下面的检查。
	// 通用的 requestValidationOptionalNumbers(…, true, …) 要求非负整数，会抢在
	// 区间检查之前把 -1 拒掉，而 -1 是官方 SDK 的 AUTOMATIC、也是本文档承诺支持的
	// 取值（docs/gemini.md 写明支持 0 或 -1），于是按文档使用的客户端直接拿到 400
	// 且请求根本到不了上游（2026-09-30 深度体检发现）。
	if raw := thinking["thinkingBudget"]; raw != nil {
		number, ok := raw.(json.Number)
		if !ok {
			return false, fmt.Errorf("generationConfig.thinkingConfig.thinkingBudget must be a number")
		}
		if _, err := number.Int64(); err != nil {
			return false, fmt.Errorf("generationConfig.thinkingConfig.thinkingBudget must be an integer")
		}
	}
	level := strings.ToLower(stringField(thinking, "thinkingLevel"))
	if level != "" && level != "low" && level != "medium" && level != "high" {
		return false, fmt.Errorf("generationConfig.thinkingConfig.thinkingLevel must be low, medium or high")
	}
	include := thinking["includeThoughts"] == true
	if raw := thinking["thinkingBudget"]; raw != nil {
		budget, err := raw.(json.Number).Int64()
		if err != nil || budget < -1 || budget > 0 {
			return false, fmt.Errorf("generationConfig.thinkingConfig.thinkingBudget supports 0 or -1; an exact positive budget cannot be enforced by this upstream")
		}
		if level != "" {
			return false, fmt.Errorf("generationConfig.thinkingConfig cannot combine thinkingBudget and thinkingLevel")
		}
		if budget == 0 {
			chat["reasoning_effort"] = "none"
			chat["thinking"] = map[string]any{"type": "disabled"}
			include = false
		}
	}
	if level != "" {
		chat["reasoning_effort"] = level
	}
	return include, nil
}

func geminiToolConfig(value any, chat map[string]any) error {
	if value == nil {
		return nil
	}
	config, err := requestValidationObject(value, "toolConfig")
	if err != nil {
		return err
	}
	if err := geminiOnlyFields(config, "toolConfig", "functionCallingConfig"); err != nil {
		return err
	}
	calling, err := requestValidationObject(config["functionCallingConfig"], "toolConfig.functionCallingConfig")
	if err != nil {
		return err
	}
	if err := geminiOnlyFields(calling, "toolConfig.functionCallingConfig", "mode", "allowedFunctionNames"); err != nil {
		return err
	}
	if err := requestValidationOptionalStrings(calling, "toolConfig.functionCallingConfig", "mode"); err != nil {
		return err
	}
	mode := stringField(calling, "mode")
	if mode == "" {
		mode = "AUTO"
	}
	allowed := map[string]bool{}
	if raw := calling["allowedFunctionNames"]; raw != nil {
		if err := requestValidationStringArray(raw, "toolConfig.functionCallingConfig.allowedFunctionNames"); err != nil {
			return err
		}
		for _, rawName := range raw.([]any) {
			name := rawName.(string)
			if name == "" || allowed[name] {
				return fmt.Errorf("toolConfig.functionCallingConfig.allowedFunctionNames must contain unique nonempty names")
			}
			allowed[name] = true
		}
	}
	tools := responseArray(chat["tools"])
	if len(allowed) > 0 {
		if mode != "ANY" {
			return fmt.Errorf("toolConfig.functionCallingConfig.allowedFunctionNames requires mode ANY")
		}
		selected := []any{}
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			if allowed[stringField(function, "name")] {
				selected = append(selected, raw)
			}
		}
		if len(selected) != len(allowed) {
			return fmt.Errorf("toolConfig.functionCallingConfig.allowedFunctionNames contains an undeclared function")
		}
		tools = selected
		chat["tools"] = selected
	}
	switch mode {
	case "AUTO":
		chat["tool_choice"] = "auto"
	case "NONE":
		chat["tool_choice"] = "none"
	case "ANY":
		if len(tools) == 0 {
			return fmt.Errorf("toolConfig.functionCallingConfig mode ANY requires a declared function")
		}
		chat["tool_choice"] = "required"
		if len(allowed) == 1 {
			for name := range allowed {
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	default:
		return fmt.Errorf("toolConfig.functionCallingConfig.mode must be AUTO, NONE or ANY")
	}
	return nil
}
