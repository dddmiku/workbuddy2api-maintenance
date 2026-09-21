// ═══ 更新日志 ═══
// 2026-09-21：新增「叙述循环」夹具回归。用户两次贴出的推理文本分别只有 5243 与
// 5909 字符、窗口内 12–13 种短行，旧阈值（8000 字符、12 种）都刚好挡在门外，表现为
// 「模型明显在抽风，但保护没有反应」。夹具按当时的统计特征重建（同一批十来句口头禅
// 轮换、行数与字符量同量级），不含任何真实会话内容。
package upstream

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// narrationLoopFixture 读入按线上统计特征重建的夹具。夹具只复刻「十来句口头禅轮换」
// 这一形态，不包含任何真实会话文本。
func narrationLoopFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("real narration fixture missing: %v", err)
	}
	return string(raw)
}

// TestReasoningLoopGuardCatchesRealNarrationLoops 锁定两次线上漏检：真实叙述循环必须
// 命中推理侧错误码。夹具的字符数与窗口内不同短行数都落在旧阈值之外，是这次放宽阈值的
// 直接依据，因此断言里同时记录这两个量，阈值回退时会立刻失败。
func TestReasoningLoopGuardCatchesRealNarrationLoops(t *testing.T) {
	for _, name := range []string{"narration_loop_profile_a.txt", "narration_loop_profile_b.txt"} {
		t.Run(name, func(t *testing.T) {
			text := narrationLoopFixture(t, name)
			guard := &reasoningLoopGuard{}
			observed := guard.add(text)
			if observed == nil {
				observed = guard.finish()
			}
			if !IsReasoningLoopError(observed) {
				t.Fatalf("real narration loop was not detected: chars=%d unique=%d err=%v",
					utf8.RuneCountInString(text), len(guard.counts), observed)
			}
			var detailed *StreamError
			if !errors.As(observed, &detailed) || detailed.Code != ReasoningLoopErrorCode {
				t.Fatalf("narration loop used the wrong code: %+v", detailed)
			}
			// 错误里只能有计数，不能回显被监控的推理文本。
			if strings.Contains(observed.Error(), "Let me") {
				t.Fatal("narration loop error exposed reasoning text")
			}
		})
	}
}

// TestReasoningLoopGuardNarrationThresholdsAreLoadBearing 固定这次放宽的依据：两个真实
// 样本的字符数都低于旧下限 8000，窗口内不同短行数都超过旧上限 12。任何一个阈值退回，
// 上面的回归就会失效，这条测试负责把「为什么是这个数」留在代码里。
func TestReasoningLoopGuardNarrationThresholdsAreLoadBearing(t *testing.T) {
	const previousMinChars = 8000
	const previousMaxUnique = 12
	for _, name := range []string{"narration_loop_profile_a.txt", "narration_loop_profile_b.txt"} {
		t.Run(name, func(t *testing.T) {
			text := narrationLoopFixture(t, name)
			chars := utf8.RuneCountInString(text)
			if chars >= previousMinChars {
				t.Fatalf("fixture no longer sits below the previous character floor: %d", chars)
			}
			guard := &reasoningLoopGuard{}
			if err := guard.add(text); err != nil {
				// 命中即说明已经触发；此时计数就是命中点的统计。
				if !IsReasoningLoopError(err) {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if unique := len(guard.counts); unique <= previousMaxUnique {
				t.Fatalf("fixture no longer exceeds the previous unique-line cap: %d", unique)
			}
		})
	}
}

// TestReasoningLoopGuardDoesNotFireOnLegitimateRepetition 是放宽阈值的对侧回归：推理里
// 常见的「合法重复」不能因为阈值放宽而被误截。表格、CSV、状态行、列表、代码行这些
// 输出天然带重复结构，误截比漏检更糟，所以这里逐类固定下来。
func TestReasoningLoopGuardDoesNotFireOnLegitimateRepetition(t *testing.T) {
	repeat := func(count int, line func(int) string) string {
		var builder strings.Builder
		for index := 0; index < count; index++ {
			builder.WriteString(line(index))
			builder.WriteString("\n")
		}
		return builder.String()
	}
	cases := map[string]string{
		// 两行/三行交替：不同短行很少，靠「一行必须占多数」这道兜底挡住。
		"two_line_table":    repeat(400, func(i int) string { return []string{"| a | b |", "| --- | --- |"}[i%2] }),
		"three_line_table":  repeat(399, func(i int) string { return []string{"| a | b |", "| --- | --- |", "| x | y |"}[i%3] }),
		"short_alternating": repeat(400, func(i int) string { return []string{"思考中", "等待中"}[i%2] }),
		// 每一行取值不同的正常表格与数据行：不同项远超上限，天然安全。
		"real_table":  repeat(400, func(i int) string { return fmt.Sprintf("| sensor_%03d | %.2f |", i, float64(i)/3) }),
		"csv_rows":    repeat(400, func(i int) string { return fmt.Sprintf("%d,%.3f,ok", i, float64(i)/7) }),
		"test_log":    repeat(400, func(i int) string { return fmt.Sprintf("[%02d:%02d] test_%03d ... ok", i/60, i%60, i) }),
		"yaml_keys":   repeat(300, func(i int) string { return fmt.Sprintf("  setting_%03d: %d", i, i*3) }),
		"code_blocks": repeat(300, func(i int) string { return fmt.Sprintf("\thandler%d.Register(mux)", i) }),
		// 正常叙述：每句都不一样。
		"distinct_steps": repeat(300, func(i int) string {
			return fmt.Sprintf("分析第 %d 个独立步骤，检查该分支的边界条件", i)
		}),
		"bullet_list":   repeat(300, func(i int) string { return fmt.Sprintf("- 第 %d 项：说明文字各不相同", i) }),
		"english_steps": repeat(300, func(i int) string { return fmt.Sprintf("Step %d: verify precondition %d before running", i, i*2) }),
		// 长行不参与重复判定。
		"matrix_rows":   strings.Repeat(strings.Repeat("0 ", 40)+"\n", 300),
		"sql_templates": strings.Repeat("INSERT INTO measurements(sensor_id, observed_value) VALUES (1, 0);\n", 300),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			guard := &reasoningLoopGuard{}
			err := guard.add(text)
			if err == nil {
				err = guard.finish()
			}
			if IsReasoningLoopError(err) {
				t.Fatalf("legitimate repetition was cut off: chars=%d unique=%d",
					utf8.RuneCountInString(text), len(guard.counts))
			}
		})
	}
}
