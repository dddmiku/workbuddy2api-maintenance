// ═══ 更新日志 ═══
// 2026-10-13：初版——Go 版面板后端的请求日志解析层，与 panel/app.py 的 REQUEST_ROW 同口径。
//
// 这一层只做「把网关表格日志文本解析成结构化行」这一件事，不读文件、不调 docker。
// 之所以单独成文件：它是纯函数，可以直接用真实日志行做表驱动测试，锁住正则与 Python 版
// 完全一致；网关日志格式一旦漂移，这里是唯一需要跟着改的地方。
//
// ═══ 为什么每一列都必须是可选取 ═══
//
// 请求行是渐进长出来的：in=/hit= 在 2026-09-18 加入，cache= 在 2026-10-10 加入。
// 同一份日志文件里会同时存在新旧格式的行，正则必须对缺失的列宽容，否则旧行整行匹配
// 失败、被归进 other，请求日志页会显示「还没有请求记录」。
//
// 其中 cache= 的坑最深：它夹在 hit= 与 tok= 之间，一旦写成必选段，所有不含 cache= 的
// 旧行（以及网关重启前写下的行）都会匹配失败——2026-10-10 真踩过，整个请求日志页空白。
// 所以这里 (?:cache=...)? 的可选括号是**契约**，不是风格问题，改动前先看这段注释。
package panel

import (
	"regexp"
	"strings"
)

// requestRowRe 匹配网关 logging.go 写出的请求行。
//
// 每一列都是渐进加入的，且都写成可选段，保证旧格式行仍能解析（见文件头说明）：
//   - in=/hit=：2026-09-18 加入；
//   - cache=：2026-10-10 加入，**必须**夹在 hit= 与 tok= 之间且保持可选；
//   - pre=：2026-10-17 加入，夹在 uid= 与 TTFB= 之间，**必须**保持可选——
//     加上它的当天就因为写成必选让整页日志解析失败（uid 组取不到 TTFB 前缀）；
//   - read=：2026-10-17 加入，夹在 uid= 与 pre= 之间，同样必须可选；同日另有
//     旧行只有 pre=（或两者都没有）。
//   - rid=：请求 ID，仅部分行有。
//
// 各组名与 Python 版 groupdict() 的键一一对应，且与 RequestRow 的 JSON tag 同名，
// 这样解析与序列化两处不会各自漂移。Go 的 regexp 要求组名唯一，本模式满足。
// pre=/read= 用非捕获组：它们只影响展示排版，不进结构化输出，避免破坏「组名 == JSON tag」的自检。
var requestRowRe = regexp.MustCompile(`^\|\s*#(?P<seq>\d+)\s*\|\s*(?P<time>[^|]*?)\s*\|\s*(?P<model>[^|]*?)\s*\|\s*(?P<mode>[^|]*?)\s*\|\s*` +
	`(?P<status>\d+)\s*\|\s*(?:key=(?P<key>[^|]*?)\s*\|\s*)?uid=(?P<uid>[^|]*?)\s*\|\s*(?:read=[^|]*?\s*\|\s*)?(?:pre=[^|]*?\s*\|\s*)?TTFB=(?P<ttfb>[^|]*?)\s*\|\s*` +
	`(?:in=(?P<in>[^|]*?)\s*\|\s*hit=(?P<hit>[^|]*?)\s*\|\s*)?` +
	`(?:cache=(?P<cache>[^|]*?)\s*\|\s*)?` +
	`tok=(?P<tok>[^|]*?)\s*\|\s*(?P<rate>[^|]*?)\s*\|\s*total=(?P<total>[^|]*?)\s*\|\s*(?:rid=(?P<request_id>[A-Za-z0-9_]+)\s*\|\s*)?$`)

// requestRowGroups 把组名映射到 FindStringSubmatch 的下标，只在包初始化时算一次。
//
// 这样解析循环里不会每次都 SubexpIndex 线性查找组名；组名写错也会在 init 阶段就暴露
// （下标为 0 意味着整段匹配，取到的是整行，测试会立刻失败），而不是静默取到空串。
var requestRowGroups = func() map[string]int {
	groups := make(map[string]int)
	for i, name := range requestRowRe.SubexpNames() {
		if name != "" {
			groups[name] = i
		}
	}
	return groups
}()

// RequestRow 是请求日志页一行。
//
// 字段与 JSON tag 严格对应 Python 版 parse_request_log 返回的 dict 键——前端 app.js
// 直接按 it.seq / it.ttfb / it.cache 取值，键名或大小写不一致就会整列显示 undefined。
// 字段顺序也保持与 Python dict 一致，便于逐字比对两版输出。
//
// 除 request_id 外所有字段都是「正则捕获到的原始字符串」，不做数值化：面板只负责显示，
// 数值解析交给前端（compactNum 会处理 "306401" 这种带或不带千分位的串）。
type RequestRow struct {
	Seq       string `json:"seq"`
	Time      string `json:"time"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	Status    string `json:"status"`
	Key       string `json:"key"`
	UID       string `json:"uid"`
	TTFB      string `json:"ttfb"`
	In        string `json:"in"`
	Hit       string `json:"hit"`
	Cache     string `json:"cache"`
	Tok       string `json:"tok"`
	Rate      string `json:"rate"`
	Total     string `json:"total"`
	RequestID string `json:"request_id"`
}

// ParseRequestLog 把请求行解析成表格行，其余行（WARN/ERR 等）按原始顺序单独返回。
//
// 第二个返回值只保留**最后 60 行**（Python 的 other[-60:]）：面板顶部只展示少量诊断行，
// 全量回传会把响应体撑大且没人看。空行（含纯空白行）直接丢弃，不计入 other。
//
// 返回值都初始化成非 nil 空切片：Python 的 [] 序列化成 JSON 是 []，而 Go 的 nil 切片
// 会变成 null，前端对 other 做 .length 之类的判断就会踩空。
func ParseRequestLog(text string) ([]RequestRow, []string) {
	rows := make([]RequestRow, 0)
	other := make([]string, 0)

	for _, line := range splitLogLines(text) {
		// Python 是 REQUEST_ROW.match(line.strip())：先去掉首尾空白再匹配，否则行首缩进
		// 或行尾 \r 会让 ^ / $ 锚点失配。other 里保留的是原始行（未 strip）。
		match := requestRowRe.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			if strings.TrimSpace(line) != "" {
				other = append(other, line)
			}
			continue
		}

		group := func(name string) string { return match[requestRowGroups[name]] }
		rows = append(rows, RequestRow{
			Seq:       group("seq"),
			Time:      group("time"),
			Model:     group("model"),
			Mode:      group("mode"),
			Status:    group("status"),
			Key:       dashIfBlank(group("key")),
			UID:       group("uid"),
			TTFB:      group("ttfb"),
			In:        dashIfBlank(group("in")),
			Hit:       dashIfBlank(group("hit")),
			Cache:     dashIfBlank(group("cache")),
			Tok:       group("tok"),
			Rate:      group("rate"),
			Total:     group("total"),
			RequestID: group("request_id"),
		})
	}

	if len(other) > logOtherTail {
		other = other[len(other)-logOtherTail:]
	}
	return rows, other
}

// logOtherTail 是 other 的截断长度（Python: other[-60:]）。
const logOtherTail = 60

// dashIfBlank 等价于 Python 的 `(value or "").strip() or "-"`：缺列（None）、空串、
// 纯空白都归一成 "-"。前端对这几列做 cacheLabel/compactNum，空串会显示成空白格。
func dashIfBlank(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return "-"
}

// splitLogLines 按 Python str.splitlines 的口径切行：\n、\r\n、\r 都算行分隔符，
// 且分隔符本身不出现在结果里（Python 也会把它去掉）。这一点必须对上：other 保留原始行，
// 若把 \r 留在行尾，前端展示诊断行时会带出一个看不见的回车。
//
// 未覆盖 Python splitlines 的 \v \f \x1c-\x1e \x85     这几个边界——
// 网关日志是 docker logs 的文本，只会用 \n / \r\n，为它们增加分支只会让实现更难读。
func splitLogLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := make([]string, 0, 64)
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			lines = append(lines, text[start:i])
			start = i + 1
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// LOG_TAIL_FACTOR 决定为了拿到 N 条请求要往回取多少原始行。
//
// 请求行与 WARN/ERR 行混排，比例不固定；实测噪声重时请求约占一半，取 4 倍再留 200 行
// 余量，既能凑够目标条数又不会把 tail 拉得过大。
const (
	logTailFactor   = 4
	logTailHeadroom = 200
	logTailMax      = 5000
)

// RequestWindowSize 返回「想要 want 条请求，需要往回取多少原始日志行」。
//
// 上限 5000 是防止 want 被前端放大后让 docker logs --tail 拉回整份日志（面板超时 40s）。
func RequestWindowSize(want int) int {
	if size := want*logTailFactor + logTailHeadroom; size < logTailMax {
		return size
	}
	return logTailMax
}
