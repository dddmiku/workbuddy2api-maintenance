// ═══ 更新日志 ═══
// 2026-09-26：schema 中字符串形式的计数约束（minItems 等，proto3 int64 映射）转为数字，官方 JS SDK 不再被整请求拒绝。
// 2026-09-25：完整保留 Gemini 工具业务 JSON，按调用顺序配对无 ID 历史并规范函数 schema。
package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type geminiPendingCall struct{ id, name string }

func geminiHistory(value, system any) ([]any, error) {
	contents, ok := value.([]any)
	if !ok || len(contents) == 0 {
		return nil, fmt.Errorf("contents must be a nonempty array")
	}
	var messages []any
	if system != nil {
		object, err := requestValidationObject(system, "systemInstruction")
		if err != nil {
			return nil, err
		}
		if err := geminiOnlyFields(object, "systemInstruction", "role", "parts"); err != nil {
			return nil, err
		}
		if err := requestValidationOptionalStrings(object, "systemInstruction", "role"); err != nil {
			return nil, err
		}
		parts, ok := object["parts"].([]any)
		if !ok || len(parts) == 0 {
			return nil, fmt.Errorf("systemInstruction.parts must be a nonempty array")
		}
		out := make([]any, 0, len(parts))
		for i, raw := range parts {
			path := fmt.Sprintf("systemInstruction.parts[%d]", i)
			part, err := requestValidationObject(raw, path)
			if err != nil {
				return nil, err
			}
			if err := geminiOnlyFields(part, path, "text"); err != nil {
				return nil, err
			}
			if err := requestValidationString(part["text"], path+".text", false); err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "text", "text": part["text"]})
		}
		messages = append(messages, map[string]any{"role": "system", "content": out})
	}
	// Reserve explicit IDs before assigning missing IDs, including later turns.
	reserved := map[string]bool{}
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		for _, rawPart := range responseArray(content["parts"]) {
			part, _ := rawPart.(map[string]any)
			for _, field := range []string{"functionCall", "functionResponse"} {
				call, _ := part[field].(map[string]any)
				if id := stringField(call, "id"); id != "" {
					reserved[id] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	pending := []geminiPendingCall{}
	next := 1
	newID := func() string {
		for {
			id := fmt.Sprintf("gemini_call_%d", next)
			next++
			if !reserved[id] {
				reserved[id] = true
				return id
			}
		}
	}
	for i, raw := range contents {
		path := fmt.Sprintf("contents[%d]", i)
		content, err := requestValidationObject(raw, path)
		if err != nil {
			return nil, err
		}
		if err := geminiOnlyFields(content, path, "role", "parts"); err != nil {
			return nil, err
		}
		if err := requestValidationOptionalStrings(content, path, "role"); err != nil {
			return nil, err
		}
		role := stringField(content, "role")
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "model" {
			return nil, fmt.Errorf("%s.role must be user or model", path)
		}
		if role == "model" && len(pending) != 0 {
			return nil, fmt.Errorf("%s is missing results for earlier functionCall parts", path)
		}
		parts, ok := content["parts"].([]any)
		if !ok || len(parts) == 0 {
			return nil, fmt.Errorf("%s.parts must be a nonempty array", path)
		}
		out, calls := []any{}, []any{}
		var reasoning strings.Builder
		results := 0
		for pi, rawPart := range parts {
			pp := fmt.Sprintf("%s.parts[%d]", path, pi)
			part, err := requestValidationObject(rawPart, pp)
			if err != nil {
				return nil, err
			}
			if err := geminiOnlyFields(part, pp, "text", "inlineData", "functionCall", "functionResponse", "thought", "thoughtSignature"); err != nil {
				return nil, err
			}
			if err := requestValidationOptionalBools(part, pp, "thought"); err != nil {
				return nil, err
			}
			if err := requestValidationOptionalStrings(part, pp, "thoughtSignature"); err != nil {
				return nil, err
			}
			if stringField(part, "thoughtSignature") != "" {
				return nil, fmt.Errorf("%s.thoughtSignature is opaque and cannot be replayed by this upstream", pp)
			}
			kinds := 0
			for _, key := range []string{"text", "inlineData", "functionCall", "functionResponse"} {
				if _, exists := part[key]; exists {
					kinds++
				}
			}
			if kinds != 1 {
				return nil, fmt.Errorf("%s must contain exactly one supported part kind", pp)
			}
			if part["thought"] == true && (role != "model" || part["text"] == nil) {
				return nil, fmt.Errorf("%s.thought requires model text", pp)
			}
			switch {
			case part["text"] != nil:
				if len(pending) != 0 && role == "user" {
					return nil, fmt.Errorf("%s must follow all pending functionResponse parts", pp)
				}
				if err := requestValidationString(part["text"], pp+".text", false); err != nil {
					return nil, err
				}
				if part["thought"] == true {
					reasoning.WriteString(part["text"].(string))
				} else {
					out = append(out, map[string]any{"type": "text", "text": part["text"]})
				}
			case part["inlineData"] != nil:
				if role != "user" || len(pending) != 0 {
					return nil, fmt.Errorf("%s.inlineData requires user content after pending tool results", pp)
				}
				data, err := requestValidationObject(part["inlineData"], pp+".inlineData")
				if err != nil {
					return nil, err
				}
				if err := geminiOnlyFields(data, pp+".inlineData", "mimeType", "data"); err != nil {
					return nil, err
				}
				mime := stringField(data, "mimeType")
				if mime == "image/jpg" {
					mime = "image/jpeg"
				}
				image, err := anthropicPart(map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": data["data"]}}, pp, true)
				if err != nil {
					return nil, err
				}
				out = append(out, image)
			case part["functionCall"] != nil:
				if role != "model" {
					return nil, fmt.Errorf("%s.functionCall requires model role", pp)
				}
				call, err := requestValidationObject(part["functionCall"], pp+".functionCall")
				if err != nil {
					return nil, err
				}
				if err := geminiOnlyFields(call, pp+".functionCall", "id", "name", "args"); err != nil {
					return nil, err
				}
				if err := requestValidationOptionalStrings(call, pp+".functionCall", "id"); err != nil {
					return nil, err
				}
				name := stringField(call, "name")
				if err := requestValidationString(call["name"], pp+".functionCall.name", true); err != nil {
					return nil, err
				}
				id := stringField(call, "id")
				if id == "" {
					id = newID()
				}
				if strings.TrimSpace(id) == "" || seen[id] || len(seen) >= 1024 {
					return nil, fmt.Errorf("%s.functionCall.id must be unique and nonempty", pp)
				}
				args := call["args"]
				if _, present := call["args"]; !present {
					args = map[string]any{}
				}
				if _, err := requestValidationObject(args, pp+".functionCall.args"); err != nil {
					return nil, err
				}
				encoded, _ := json.Marshal(args)
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}})
				seen[id] = true
				pending = append(pending, geminiPendingCall{id, name})
			case part["functionResponse"] != nil:
				if role != "user" || len(out) != 0 {
					return nil, fmt.Errorf("%s.functionResponse must precede other user content", pp)
				}
				result, err := requestValidationObject(part["functionResponse"], pp+".functionResponse")
				if err != nil {
					return nil, err
				}
				if err := geminiOnlyFields(result, pp+".functionResponse", "id", "name", "response"); err != nil {
					return nil, err
				}
				if err := requestValidationOptionalStrings(result, pp+".functionResponse", "id"); err != nil {
					return nil, err
				}
				if err := requestValidationString(result["name"], pp+".functionResponse.name", true); err != nil {
					return nil, err
				}
				if _, err := requestValidationObject(result["response"], pp+".functionResponse.response"); err != nil {
					return nil, err
				}
				id, name := stringField(result, "id"), stringField(result, "name")
				match := -1
				for i, call := range pending {
					if (id != "" && id == call.id && name == call.name) || (id == "" && name == call.name) {
						match = i
						break
					}
				}
				if match < 0 {
					return nil, fmt.Errorf("%s.functionResponse has no matching pending call", pp)
				}
				encoded, _ := json.Marshal(result["response"])
				// response is a business object, not a media/content-block envelope.
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": pending[match].id, "content": string(encoded)})
				pending = append(pending[:match], pending[match+1:]...)
				results++
			default:
				return nil, fmt.Errorf("%s must contain a supported non-null part", pp)
			}
		}
		if len(out) != 0 || len(calls) != 0 || reasoning.Len() > 0 {
			message := map[string]any{"role": role, "content": out}
			if role == "model" {
				message["role"] = "assistant"
			}
			if len(calls) != 0 {
				message["tool_calls"] = calls
			}
			if reasoning.Len() > 0 {
				message["reasoning_content"] = reasoning.String()
			}
			messages = append(messages, message)
		} else if results == 0 {
			return nil, fmt.Errorf("%s is empty", path)
		}
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("contents is missing results for functionCall history")
	}
	return messages, nil
}

func geminiTools(value any) ([]any, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("tools must be an array")
	}
	tools := []any{}
	seen := map[string]bool{}
	for i, raw := range items {
		path := fmt.Sprintf("tools[%d]", i)
		tool, err := requestValidationObject(raw, path)
		if err != nil {
			return nil, err
		}
		if err := geminiOnlyFields(tool, path, "functionDeclarations"); err != nil {
			return nil, err
		}
		declarations, ok := tool["functionDeclarations"].([]any)
		if !ok || len(declarations) == 0 {
			return nil, fmt.Errorf("%s.functionDeclarations must be a nonempty array", path)
		}
		for di, rawDeclaration := range declarations {
			dp := fmt.Sprintf("%s.functionDeclarations[%d]", path, di)
			d, err := requestValidationObject(rawDeclaration, dp)
			if err != nil {
				return nil, err
			}
			if err := geminiOnlyFields(d, dp, "name", "description", "parameters", "parametersJsonSchema"); err != nil {
				return nil, err
			}
			if err := requestValidationString(d["name"], dp+".name", true); err != nil {
				return nil, err
			}
			if err := requestValidationOptionalStrings(d, dp, "description"); err != nil {
				return nil, err
			}
			name := stringField(d, "name")
			if seen[name] || len(seen) >= 1024 {
				return nil, fmt.Errorf("%s.name repeats a function declaration", dp)
			}
			seen[name] = true
			params := d["parameters"]
			if params != nil && d["parametersJsonSchema"] != nil {
				return nil, fmt.Errorf("%s cannot combine parameters and parametersJsonSchema", dp)
			}
			if d["parametersJsonSchema"] != nil {
				params = d["parametersJsonSchema"]
			}
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			schema, err := geminiSchema(params, dp+".parameters")
			if err != nil {
				return nil, err
			}
			// Gemini declares a parameter schema without an OpenAI strict flag.
			// Activate the existing terminal schema checker so a valid JSON object
			// with invalid field types cannot become an executable functionCall.
			function := map[string]any{"name": name, "parameters": schema, "strict": true}
			if d["description"] != nil {
				function["description"] = d["description"]
			}
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
	}
	return tools, nil
}

// Only schema-bearing nodes recurse. Defaults/enums/const are business JSON;
// a property named "type" inside them must not be rewritten as a schema type.
func geminiSchema(value any, path string) (map[string]any, error) {
	return geminiSchemaDepth(value, path, 0)
}

func geminiSchemaDepth(value any, path string, depth int) (map[string]any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("%s exceeds the schema depth limit of 64", path)
	}
	source, err := requestValidationObject(value, path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
		switch key {
		case "type":
			switch v := value.(type) {
			case string:
				v = strings.ToLower(v)
				if v != "object" && v != "array" && v != "string" && v != "number" && v != "integer" && v != "boolean" && v != "null" {
					return nil, fmt.Errorf("%s.type is not supported", path)
				}
				out[key] = v
			case []any:
				if err := requestValidationStringArray(v, path+".type"); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("%s.type must be a schema type", path)
			}
		case "minItems", "maxItems", "minLength", "maxLength", "minProperties", "maxProperties":
			// Gemini 的 int64 字段按 proto3 JSON 映射可以是字符串（@google/genai 会发
			// "minItems":"1"）；按 JSON Schema 需要数字，否则本地 schema 编译整请求 400。
			if text, ok := value.(string); ok {
				count, err := strconv.ParseUint(strings.TrimSpace(text), 10, 63)
				if err != nil {
					return nil, fmt.Errorf("%s.%s must be a non-negative integer", path, key)
				}
				out[key] = json.Number(strconv.FormatUint(count, 10))
			}
		case "properties", "$defs", "definitions", "patternProperties":
			members, err := requestValidationObject(value, path+"."+key)
			if err != nil {
				return nil, err
			}
			converted := make(map[string]any, len(members))
			for name, member := range members {
				converted[name], err = geminiSchemaDepth(member, path+"."+key+"."+name, depth+1)
				if err != nil {
					return nil, err
				}
			}
			out[key] = converted
		case "items", "not", "propertyNames", "contains", "if", "then", "else":
			out[key], err = geminiSchemaDepth(value, path+"."+key, depth+1)
			if err != nil {
				return nil, err
			}
		case "additionalProperties":
			if _, ok := value.(bool); !ok {
				out[key], err = geminiSchemaDepth(value, path+"."+key, depth+1)
				if err != nil {
					return nil, err
				}
			}
		case "anyOf", "oneOf", "allOf", "prefixItems":
			children, ok := value.([]any)
			if !ok || len(children) == 0 {
				return nil, fmt.Errorf("%s.%s must be a nonempty schema array", path, key)
			}
			converted := make([]any, 0, len(children))
			for i, child := range children {
				result, err := geminiSchemaDepth(child, fmt.Sprintf("%s.%s[%d]", path, key, i), depth+1)
				if err != nil {
					return nil, err
				}
				converted = append(converted, result)
			}
			out[key] = converted
		}
	}
	if nullable, exists := source["nullable"]; exists {
		if _, ok := nullable.(bool); !ok {
			return nil, fmt.Errorf("%s.nullable must be a boolean", path)
		}
		delete(out, "nullable")
		if nullable == true {
			if kind, ok := out["type"].(string); ok && kind != "null" {
				out["type"] = []any{kind, "null"}
			} else if len(out) > 0 {
				// 没有单一 type 的联合形状（@google/genai 会为可空联合发
				// {"nullable":true,"anyOf":[…]}，枚举发 {"nullable":true,"enum":[…]}）：
				// 此前一律 400，而这是 SDK 正常产出的合法 schema，客户端根本到不了上游。
				// 用 anyOf 包一层并追加 {"type":"null"} 分支，语义等价、且是合法
				// JSON Schema（strict 校验走 santhosh jsonschema，支持 anyOf）。
				inner := make(map[string]any, len(out))
				for key, value := range out {
					inner[key] = value
				}
				out = map[string]any{"anyOf": []any{inner, map[string]any{"type": "null"}}}
			} else {
				// 只有一个孤零零的 nullable：没有任何可空化的结构，保持原样拒绝。
				return nil, fmt.Errorf("%s.nullable requires a schema to make nullable", path)
			}
		}
	}
	return out, nil
}
