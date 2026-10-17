// ═══ 更新日志 ═══
// 2026-10-13：初版——锁住 Go 版 ParseRequestLog 与 panel/app.py 的 REQUEST_ROW 同口径。
//
// 为什么这组测试必须存在：日志格式是**渐进长出来**的，同一份日志里新行旧行混排。
// 只要正则里某个可选取被写成必选（尤其 cache=），旧行会整行失配、请求日志页直接空白，
// 而这类回归在类型系统里毫无痕迹——只能靠真实格式的行钉住。
//
// 用例里的行都是真实网关日志的形状（含中文 key、tok/s、rid=），不是构造出来的玩具串。
package panel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// logLineFull 是当前格式的完整行，各列齐全。
const logLineFull = `| #012 | 22:04:21 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=3414ms | in=306401 | hit=298112 | cache=prev=305900 | tok=110 | 34.3tok/s | total=3.4s |`

func TestParseRequestLog(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		want      []RequestRow
		wantOther []string
	}{
		{
			name: "带 read= 与 pre= 的当前格式",
			text: `| #018 | 22:05:12 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | read=312ms | pre=35ms | TTFB=3414ms | in=306401 | hit=298112 | cache=prev=305900 | tok=110 | 34.3tok/s | total=3.4s |`,
			want: []RequestRow{{
				Seq: "018", Time: "22:05:12", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "3414ms",
				In: "306401", Hit: "298112", Cache: "prev=305900",
				Tok: "110", Rate: "34.3tok/s", Total: "3.4s", RequestID: "",
			}},
		},
		{
			name: "带 pre= 的当前格式",
			text: `| #016 | 22:05:10 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | pre=35ms | TTFB=3414ms | in=306401 | hit=298112 | cache=prev=305900 | tok=110 | 34.3tok/s | total=3.4s |`,
			want: []RequestRow{{
				Seq: "016", Time: "22:05:10", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "3414ms",
				In: "306401", Hit: "298112", Cache: "prev=305900",
				Tok: "110", Rate: "34.3tok/s", Total: "3.4s", RequestID: "",
			}},
		},
		{
			name: "完整行",
			text: logLineFull,
			want: []RequestRow{{
				Seq: "012", Time: "22:04:21", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "3414ms",
				In: "306401", Hit: "298112", Cache: "prev=305900",
				Tok: "110", Rate: "34.3tok/s", Total: "3.4s", RequestID: "",
			}},
			wantOther: []string{},
		},
		{
			name: "旧格式_无 cache=",
			text: `| #013 | 22:04:25 | global:fast | stream | 200 | key=团队 B | uid=2f15f45e | TTFB=1200ms | in=1000 | hit=0 | tok=80 | 66.7tok/s | total=1.2s |`,
			want: []RequestRow{{
				Seq: "013", Time: "22:04:25", Model: "global:fast", Mode: "stream", Status: "200",
				Key: "团队 B", UID: "2f15f45e", TTFB: "1200ms",
				In: "1000", Hit: "0", Cache: "-",
				Tok: "80", Rate: "66.7tok/s", Total: "1.2s", RequestID: "",
			}},
			wantOther: []string{},
		},
		{
			name: "更旧格式_无 key= 且无 in=/hit=",
			text: `| #014 | 22:04:30 | global:deep | stream | 200 | uid=3a26a56f | TTFB=900ms | tok=50 | 55.6tok/s | total=0.9s |`,
			want: []RequestRow{{
				Seq: "014", Time: "22:04:30", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "-", UID: "3a26a56f", TTFB: "900ms",
				In: "-", Hit: "-", Cache: "-",
				Tok: "50", Rate: "55.6tok/s", Total: "0.9s", RequestID: "",
			}},
			wantOther: []string{},
		},
		{
			name: "带 rid=",
			text: `| #015 | 22:04:35 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=500ms | in=200 | hit=100 | cache=cold | tok=20 | 40.0tok/s | total=0.5s | rid=abc123_DEF |`,
			want: []RequestRow{{
				Seq: "015", Time: "22:04:35", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "500ms",
				In: "200", Hit: "100", Cache: "cold",
				Tok: "20", Rate: "40.0tok/s", Total: "0.5s", RequestID: "abc123_DEF",
			}},
			wantOther: []string{},
		},
		{
			name: "空 key= 与空列都归一成 -",
			text: `| #016 | 22:04:40 | global:deep | stream | 200 | key=   | uid=1e04e34d | TTFB=300ms | in= | hit= | cache= | tok=10 | 33.3tok/s | total=0.3s |`,
			want: []RequestRow{{
				Seq: "016", Time: "22:04:40", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "-", UID: "1e04e34d", TTFB: "300ms",
				In: "-", Hit: "-", Cache: "-",
				Tok: "10", Rate: "33.3tok/s", Total: "0.3s", RequestID: "",
			}},
			wantOther: []string{},
		},
		{
			name: "WARN/ERR 行落进 other 且保持原始顺序",
			text: strings.Join([]string{
				`| #017 | 22:04:41 | global:deep | stream | 200 | key=团队 A | uid=1e04e34d | TTFB=100ms | tok=5 | 50.0tok/s | total=0.1s |`,
				`2026-10-13 22:04:42 WARN upstream retry attempt=2`,
				``,
				`   `,
				`2026-10-13 22:04:43 ERR upstream 502 model=global:deep`,
			}, "\n"),
			want: []RequestRow{{
				Seq: "017", Time: "22:04:41", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "100ms",
				In: "-", Hit: "-", Cache: "-",
				Tok: "5", Rate: "50.0tok/s", Total: "0.1s", RequestID: "",
			}},
			// 空行与纯空白行被丢弃，不进 other。
			wantOther: []string{
				`2026-10-13 22:04:42 WARN upstream retry attempt=2`,
				`2026-10-13 22:04:43 ERR upstream 502 model=global:deep`,
			},
		},
		{
			name: "行首缩进与 CRLF 仍能匹配",
			text: "  " + logLineFull + "  \r\n",
			want: []RequestRow{{
				Seq: "012", Time: "22:04:21", Model: "global:deep", Mode: "stream", Status: "200",
				Key: "团队 A", UID: "1e04e34d", TTFB: "3414ms",
				In: "306401", Hit: "298112", Cache: "prev=305900",
				Tok: "110", Rate: "34.3tok/s", Total: "3.4s", RequestID: "",
			}},
			wantOther: []string{},
		},
		{
			name:      "空输入返回非 nil 空切片",
			text:      "",
			want:      []RequestRow{},
			wantOther: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, other := ParseRequestLog(tc.text)
			if rows == nil || other == nil {
				t.Fatalf("返回值必须是空切片而不是 nil：rows=%v other=%v", rows, other)
			}
			if len(rows) != len(tc.want) {
				t.Fatalf("rows 条数 = %d，want %d：%+v", len(rows), len(tc.want), rows)
			}
			for i := range tc.want {
				if rows[i] != tc.want[i] {
					t.Errorf("rows[%d] = %+v\n           want %+v", i, rows[i], tc.want[i])
				}
			}
			if len(other) != len(tc.wantOther) {
				t.Fatalf("other = %q，want %q", other, tc.wantOther)
			}
			for i := range tc.wantOther {
				if other[i] != tc.wantOther[i] {
					t.Errorf("other[%d] = %q，want %q", i, other[i], tc.wantOther[i])
				}
			}
		})
	}
}

// TestParseRequestLogOtherTail60 证明 other 只保留**最后 60 行**（Python: other[-60:]）。
//
// 用 65 行不匹配的行 + 中间插入空行：空行被丢弃、不计入 60，且被丢掉的应是**最早的 5 行**，
// 留下的是第 5..64 行——只断言「长度是 60」不够，必须断言内容，否则截错方向也会通过。
func TestParseRequestLogOtherTail60(t *testing.T) {
	const total = 65
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, fmt.Sprintf("2026-10-13 22:05:%02d WARN noise line %d", i%60, i))
	}
	// 空行穿插其间，确认被丢弃且不占 60 的名额。
	text := strings.Join(lines, "\n") + "\n\n   \n"

	rows, other := ParseRequestLog(text)
	if len(rows) != 0 {
		t.Fatalf("不应解析出请求行，got %+v", rows)
	}
	if len(other) != 60 {
		t.Fatalf("other 条数 = %d，want 60", len(other))
	}
	if other[0] != lines[total-60] {
		t.Errorf("other 应保留最后 60 行，other[0] = %q，want %q", other[0], lines[total-60])
	}
	if other[59] != lines[total-1] {
		t.Errorf("other[59] = %q，want %q", other[59], lines[total-1])
	}
}

func TestRequestWindowSize(t *testing.T) {
	cases := []struct {
		want int
		in   int
		out  int
	}{
		{want: 0, in: 0, out: 200},
		{want: 1, in: 1, out: 204},
		{want: 50, in: 50, out: 400},
		{want: 1200, in: 1200, out: 5000}, // 1200*4+200=5000，正好触上限
		{want: 5000, in: 5000, out: 5000}, // 远超上限，被夹到 5000
	}
	for _, tc := range cases {
		if got := RequestWindowSize(tc.want); got != tc.out {
			t.Errorf("RequestWindowSize(%d) = %d，want %d", tc.want, got, tc.out)
		}
	}
}

// TestRequestRowJSONKeys 锁死 JSON tag 与 Python dict 键逐字一致。
//
// 前端 app.js 直接按 it.seq / it.ttfb / it.cache 取列，tag 写错只会显示 undefined，
// 不会有任何报错——所以在这里把键集合固定下来。
func TestRequestRowJSONKeys(t *testing.T) {
	raw, err := json.Marshal(RequestRow{})
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}

	want := []string{
		"seq", "time", "model", "mode", "status", "key", "uid",
		"ttfb", "in", "hit", "cache", "tok", "rate", "total", "request_id",
	}
	if len(got) != len(want) {
		t.Fatalf("JSON 键数 = %d，want %d：%s", len(got), len(want), raw)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("JSON 缺少键 %q：%s", key, raw)
		}
	}
}
