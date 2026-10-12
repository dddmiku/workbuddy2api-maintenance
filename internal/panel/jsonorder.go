// ═══ 更新日志 ═══
// 2026-10-13：初版——保序 JSON 读写，供面板改写 config.json 时保持键序与缩进。
//
// 为什么不能用 map[string]any：面板要改的是**运维手写的配置文件**，只动
// schedule.<key>_enabled 一个键。Go 的 map 序列化会按键名排序输出，于是「改一个
// 开关」会把整份文件的键序打乱——diff 里全是噪声，运维再也看不出自己改了什么，
// 也不敢再让工具碰这个文件。Python 版靠 dict 的插入序天然保序，这里必须自己实现。
//
// 只实现面板真正需要的子集：对象、数组、字符串、数字、布尔、null。数字一律以
// json.Number 原样保留文本，避免 int64→float64 往返把 1000000 写成 1e+06。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// orderedObject 是保序 JSON 对象：keys 记录插入顺序，vals 存值。
//
// 重复键按 Python 的 json 语义处理：后者覆盖值、但保留先出现的位置。
type orderedObject struct {
	keys []string
	vals map[string]any
}

func newOrderedObject() *orderedObject {
	return &orderedObject{vals: map[string]any{}}
}

// Set 写入一个键值；键已存在时只覆盖值，不改动它在文件里的位置。
func (o *orderedObject) Set(key string, value any) {
	if _, exists := o.vals[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// Get 按键取值。
func (o *orderedObject) Get(key string) (any, bool) {
	value, ok := o.vals[key]
	return value, ok
}

// Len 返回键数量。
func (o *orderedObject) Len() int { return len(o.keys) }

// parseOrderedJSON 解析 JSON，对象一律解成 *orderedObject。
func parseOrderedJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeOrderedValue(decoder)
	if err != nil {
		return nil, err
	}
	// 文档后还有非空白内容说明输入畸形（两个 JSON 拼在一起之类），必须报错而不是
	// 静默取第一个——配置文件被这么写坏时，读一半比读失败更危险。
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("JSON 文档结尾有多余内容")
	}
	return value, nil
}

func decodeOrderedValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := newOrderedObject()
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON 对象的键不是字符串")
			}
			value, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			object.Set(key, value)
		}
		if _, err := decoder.Token(); err != nil { // 消费 '}'
			return nil, err
		}
		return object, nil
	case '[':
		items := []any{}
		for decoder.More() {
			value, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		if _, err := decoder.Token(); err != nil { // 消费 ']'
			return nil, err
		}
		return items, nil
	}
	return nil, fmt.Errorf("意外的 JSON 分隔符 %v", delim)
}

// marshalOrdered 按 Python json.dump(indent=N, ensure_ascii=False) 的排版输出。
//
// 对齐的是**字节形态**：缩进宽度、`": "` 分隔符、空容器写成 `{}` / `[]`、
// 非 ASCII 原样输出。面板改完配置文件后要和原文件做 diff，排版差一个空格都是噪声。
func marshalOrdered(value any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrdered(&buf, value, indent, ""); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeOrdered(buf *bytes.Buffer, value any, indent, prefix string) error {
	switch v := value.(type) {
	case *orderedObject:
		if len(v.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		inner := prefix + indent
		buf.WriteString("{\n")
		for i, key := range v.keys {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(inner)
			writeJSONString(buf, key)
			buf.WriteString(": ")
			if err := writeOrdered(buf, v.vals[key], indent, inner); err != nil {
				return err
			}
		}
		buf.WriteString("\n")
		buf.WriteString(prefix)
		buf.WriteString("}")
		return nil
	case []any:
		if len(v) == 0 {
			buf.WriteString("[]")
			return nil
		}
		inner := prefix + indent
		buf.WriteString("[\n")
		for i, item := range v {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(inner)
			if err := writeOrdered(buf, item, indent, inner); err != nil {
				return err
			}
		}
		buf.WriteString("\n")
		buf.WriteString(prefix)
		buf.WriteString("]")
		return nil
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		buf.WriteString(strconv.FormatBool(v))
		return nil
	case json.Number:
		buf.WriteString(v.String())
		return nil
	case string:
		writeJSONString(buf, v)
		return nil
	default:
		// 兜底：调用方直接塞进来的 Go 原生值（int/float64/…）走标准库编码。
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	}
}

// writeJSONString 写一个 JSON 字符串字面量（ensure_ascii=False 形态）。
//
// 不用 json.Marshal：它会转义 < > & 为 < 之类（防 XSS 的默认行为），
// 而 Python 的 json.dump 不会。配置文件里出现 URL 或比较运算符时，两边排版会分叉。
func writeJSONString(buf *bytes.Buffer, text string) {
	buf.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// orderedChild 取子对象；不存在或不是对象时返回 nil。
func orderedChild(value any, key string) *orderedObject {
	object, ok := value.(*orderedObject)
	if !ok {
		return nil
	}
	child, ok := object.Get(key)
	if !ok {
		return nil
	}
	nested, _ := child.(*orderedObject)
	return nested
}
