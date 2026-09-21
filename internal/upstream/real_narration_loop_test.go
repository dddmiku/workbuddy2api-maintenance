// ═══ 更新日志 ═══
// 2026-09-22：判定改为「只看重复形态、不设字符量下限」，并新增两道防线回归：
// 长行不占窗口格位、``` 代码围栏内的行不进入窗口。原来的「字符阈值加载性」断言
// 随字符下限一起删除。
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

// TestReasoningLoopGuardIgnoresCharacterVolume 固定「判定与推理字数无关」这条语义：
// 同样一段循环，前面塞进大量正常推理（几万字符）之后，命中点只由重复形态决定，
// 不因为总量变大而提前或延后；反过来，很短的循环（几百字符）也必须被抓住。
func TestReasoningLoopGuardIgnoresCharacterVolume(t *testing.T) {
	loop := strings.Repeat("Let me run.\n", 40)

	// 很短的循环：总量远低于历史上用过的任何字符下限，仍必须命中。
	short := &reasoningLoopGuard{}
	err := short.add(loop)
	if err == nil {
		err = short.finish()
	}
	if !IsReasoningLoopError(err) {
		t.Fatalf("short loop was not detected: chars=%d", utf8.RuneCountInString(loop))
	}

	// 前面加两万字符正常推理：命中点应落在重复开始之后不久，而不是被总量推迟。
	var builder strings.Builder
	for index := 0; index < 2000; index++ {
		fmt.Fprintf(&builder, "分析第 %04d 个独立步骤\n", index)
	}
	builder.WriteString(loop)
	late := &reasoningLoopGuard{}
	var hit error
	runes := []rune(builder.String())
	for offset := 0; offset < len(runes); offset += 64 {
		end := offset + 64
		if end > len(runes) {
			end = len(runes)
		}
		if err := late.add(string(runes[offset:end])); err != nil {
			hit = err
			break
		}
	}
	if !IsReasoningLoopError(hit) {
		t.Fatalf("late loop was not detected: %v", hit)
	}
}

// TestReasoningLoopGuardLongLinesDoNotDiluteWindow 是根因回归：长行永远不可能重复，
// 不能占窗口格位。夹具模拟线上真实形态——循环短行之间夹着大量长行，旧语义下覆盖率
// 被稀释到 95% 以下，保护要等整段结束才勉强命中。
func TestReasoningLoopGuardLongLinesDoNotDiluteWindow(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < 400; index++ {
		// 每两行重复短行夹一条长行：长行占约 1/3 的非空行，与线上 19%–43% 同量级。
		if index%3 == 2 {
			fmt.Fprintf(&builder, "Let me examine the branch number %d in full detail before proceeding\n", index)
			continue
		}
		builder.WriteString("Let me run.\n")
	}
	text := builder.String()
	guard := &reasoningLoopGuard{}
	err := guard.add(text)
	if err == nil {
		err = guard.finish()
	}
	if !IsReasoningLoopError(err) {
		t.Fatalf("loop diluted by long lines was not detected: chars=%d unique=%d",
			utf8.RuneCountInString(text), len(guard.counts))
	}
	// 命中时窗口里应当全是短行：长行不该占格位。
	if guard.size != reasoningLoopWindow {
		t.Fatalf("window size drifted: %d", guard.size)
	}
}

// TestReasoningLoopGuardIgnoresFencedCode 是误报防线回归：``` 围栏内的代码行天然
// 重复（`}`、`...`、`status = {`），实测真实推理里足以骗过覆盖率判定，必须排除。
func TestReasoningLoopGuardIgnoresFencedCode(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < 200; index++ {
		// 围栏外的说明句必须各不相同：如果它们也重复，命中就是正确行为，
		// 测不到「围栏内被排除」这件事。
		fmt.Fprintf(&builder, "先看一下第 %d 处实现的结构：\n", index)
		builder.WriteString("```python\n")
		builder.WriteString("status = {\n")
		builder.WriteString("...\n")
		builder.WriteString("'problems': problems,\n")
		builder.WriteString("}\n")
		builder.WriteString("```\n")
	}
	text := builder.String()
	guard := &reasoningLoopGuard{}
	err := guard.add(text)
	if err == nil {
		err = guard.finish()
	}
	if IsReasoningLoopError(err) {
		t.Fatalf("fenced code was mistaken for a loop: unique=%d", len(guard.counts))
	}
}

// TestReasoningLoopGuardReopensOnUnclosedFence 锁定围栏兜底：上游如果输出一个始终不闭合的
// ```，检测不能被永久关掉；超过上限后短行重新进入窗口，循环照常命中。
func TestReasoningLoopGuardReopensOnUnclosedFence(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("```python\n")
	for index := 0; index < reasoningLoopMaxFencedLines+2; index++ {
		builder.WriteString("status = {\n")
	}
	builder.WriteString(strings.Repeat("Let me run.\n", 40))
	guard := &reasoningLoopGuard{}
	err := guard.add(builder.String())
	if err == nil {
		err = guard.finish()
	}
	if !IsReasoningLoopError(err) {
		t.Fatalf("unclosed fence disabled detection forever: err=%v", err)
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
