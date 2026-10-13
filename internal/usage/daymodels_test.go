// ═══ 更新日志 ═══
// 2026-10-13：新增。锁住「天 × 模型」两维桶——面板按日期筛选时模型拆分必须仍可用，
// 且跨进程增量合并不得丢这一维。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDayModelsAccumulatePerDayAndModel 两维桶按「天 + 模型」分别累加。
//
// 为什么需要它：面板勾选某一天后，「模型拆分」按钮此前整列变成「—」点不动
// （2026-10-13 用户实测截图）——因为天桶只有总量、没有模型维度。修法是账本多记一维，
// 这条用例锁住那一维确实按两轴分桶，而不是退化成总量。
func TestDayModelsAccumulatePerDayAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	day1 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	s.Record("key", "client", "masked", "cn:glm-5.3", 100, 10, 0, 1.5, true, day1)
	s.Record("key", "client", "masked", "cn:glm-5.3", 200, 20, 0, 2.5, true, day1)
	s.Record("key", "client", "masked", "cn:hy3", 300, 30, 0, 0, true, day1)
	s.Record("key", "client", "masked", "cn:glm-5.3", 400, 40, 0, 9.0, true, day2)

	snapshot := s.Snapshot()
	if len(snapshot.Keys) != 1 {
		t.Fatalf("keys=%d", len(snapshot.Keys))
	}
	byDay := map[string]map[string]Totals{}
	for _, day := range snapshot.Keys[0].DayModels {
		byDay[day.Day] = map[string]Totals{}
		for _, model := range day.Models {
			byDay[day.Day][model.Model] = model.Totals
		}
	}
	d13 := byDay["2026-10-13"]
	if d13 == nil {
		t.Fatalf("缺少 2026-10-13 的两维桶：%+v", byDay)
	}
	if got := d13["cn:glm-5.3"]; got.Requests != 2 || got.PromptTokens != 300 || got.TotalTokens != 330 {
		t.Errorf("13 日 cn:glm-5.3 = %+v，期望 2 次 / 300 输入 / 330 合计", got)
	}
	if got := d13["cn:hy3"]; got.Requests != 1 || got.PromptTokens != 300 {
		t.Errorf("13 日 cn:hy3 = %+v，期望 1 次 / 300 输入", got)
	}
	if got := d13["cn:glm-5.3"].Credit; got != 4.0 {
		t.Errorf("13 日 cn:glm-5.3 积分 = %v，期望 4（1.5+2.5）", got)
	}
	d12 := byDay["2026-10-12"]
	if d12 == nil || d12["cn:glm-5.3"].Requests != 1 {
		t.Fatalf("12 日应只有 cn:glm-5.3 一次：%+v", d12)
	}
	// 12 日不应混入 13 日的模型：两轴必须各自独立。
	if _, ok := d12["cn:hy3"]; ok {
		t.Error("12 日不该出现只在 13 日用过的 cn:hy3")
	}
}

// TestDayModelsSurvivePersistAndReload 两维桶必须落盘并能读回。
//
// 面板读的是快照，而快照 = 盘上账本 + 本进程增量；这一维若没写进落盘结构，
// 进程重启后面板就再也给不出筛选态的模型拆分了。
func TestDayModelsSurvivePersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	s.Record("key", "client", "masked", "cn:glm-5.3", 100, 10, 0, 1.5, true, at)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys, _ := doc["keys"].(map[string]any)
	entry, _ := keys["key"].(map[string]any)
	if _, ok := entry["day_models"]; !ok {
		t.Fatalf("落盘账本缺少 day_models 字段：%v", entry)
	}

	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if len(snapshot.Keys) != 1 || len(snapshot.Keys[0].DayModels) != 1 {
		t.Fatalf("重开后两维桶丢失：%+v", snapshot.Keys)
	}
	day := snapshot.Keys[0].DayModels[0]
	if day.Day != "2026-10-13" || len(day.Models) != 1 || day.Models[0].Model != "cn:glm-5.3" {
		t.Fatalf("重开后两维桶内容不对：%+v", day)
	}
	if day.Models[0].Totals.PromptTokens != 100 {
		t.Errorf("重开后输入 token = %d，期望 100", day.Models[0].Totals.PromptTokens)
	}
}

// TestDayModelsMergeAcrossInstances 两个进程各自记的同一「天 × 模型」必须相加，不能丢。
//
// 热更新期间新旧进程会短暂同时持账本；两维桶是新加的维度，若增量/合并路径漏掉它，
// 表现为「重启后当天的模型拆分少了一截」——这类丢数据不会报错，只会静默少算。
func TestDayModelsMergeAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	first, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.Local)
	first.Record("key", "client", "masked", "cn:glm-5.3", 100, 10, 0, 1.0, true, at)
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}

	// 第二个实例读同一文件，各记一笔，然后各自落盘——两边都不能丢。
	second, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first.Record("key", "client", "masked", "cn:glm-5.3", 200, 20, 0, 2.0, true, at)
	second.Record("key", "client", "masked", "cn:glm-5.3", 400, 40, 0, 4.0, true, at)
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := second.Flush(); err != nil {
		t.Fatal(err)
	}
	second.Close()
	first.Close()

	reopened, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if len(snapshot.Keys) != 1 || len(snapshot.Keys[0].DayModels) != 1 {
		t.Fatalf("两维桶缺失：%+v", snapshot.Keys)
	}
	got := snapshot.Keys[0].DayModels[0].Models[0].Totals
	if got.Requests != 3 {
		t.Fatalf("合并后请求数 = %d，期望 3（1+1+1，两实例都不丢）", got.Requests)
	}
	if got.PromptTokens != 700 {
		t.Errorf("合并后输入 token = %d，期望 700（100+200+400）", got.PromptTokens)
	}
	if got.Credit != 7.0 {
		t.Errorf("合并后积分 = %v，期望 7（1+2+4）", got.Credit)
	}
}

// TestDayModelsTrimmedWithDayBuckets 两维桶的天数与天桶同上限，不无限增长。
func TestDayModelsTrimmedWithDayBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	for i := 0; i < maxDayBuckets+30; i++ {
		s.Record("key", "client", "masked", "cn:glm-5.3", 10, 1, 0, 0.1, true, base.AddDate(0, 0, i))
	}
	snapshot := s.Snapshot()
	days := snapshot.Keys[0].DayModels
	if len(days) > maxDayBuckets {
		t.Fatalf("两维桶天数 = %d，超过上限 %d", len(days), maxDayBuckets)
	}
	if len(days) == 0 {
		t.Fatal("两维桶被裁空")
	}
	// 保留的必须是最新的那批：最新的那天是 base + (maxDayBuckets+29)。
	newest := base.AddDate(0, 0, maxDayBuckets+29).In(time.Local).Format(dayKeyLayout)
	if days[0].Day != newest {
		t.Errorf("最新的天 = %s，期望 %s", days[0].Day, newest)
	}
}
