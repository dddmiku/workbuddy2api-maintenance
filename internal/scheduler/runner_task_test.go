package scheduler

// 2026-10-19 新增。锁住「runner 是一次性任务」这条契约。
//
// 用户明确要求：这些任务不用设置时间，手动执行即可。契约有三条：
//   1. 没有排程小时（面板显示「手动」）；
//   2. 不进 nextWake 候选——即使未来任何时刻都不会被自动触发；
//   3. Enabled 恒为 true，否则面板的「跑一次」按钮会被禁用。
//
// 第 2 条是最容易在后续重构里丢的：把 taskRunner 加回 nextWake 的候选循环，
// 编译照样通过、面板照样显示，只是它开始每天自动跑一遍上游事件。所以这里
// 用「扫描未来 7 天的每个唤醒点」来钉住，而不是只看一次 nextWake 的返回值。

import (
	"testing"
	"time"
)

func TestRunnerIsManualOnly(t *testing.T) {
	s := New(Config{})
	for _, info := range s.TaskSnapshot() {
		if info.Key != "runner" {
			continue
		}
		if len(info.Hours) != 0 {
			t.Fatalf("runner 不应有排程小时，实际 %v", info.Hours)
		}
		if info.NextRun != nil {
			t.Fatalf("runner 不应有下次运行时间，实际 %v", info.NextRun)
		}
		if !info.Enabled {
			t.Fatal("runner 应可手动触发（Enabled=true），否则面板按钮会被禁用")
		}
		if len(info.Realms) != 1 || info.Realms[0] != "cn" {
			t.Fatalf("runner 只服务 cn（脚本内 auth_skips_cn_task 排除 global/trae），实际 %v", info.Realms)
		}
		if info.Detail == "" {
			t.Fatal("runner 缺少说明文案，面板会显示空描述")
		}
		return
	}
	t.Fatal("任务快照里没有 runner")
}

func TestRunnerNeverScheduled(t *testing.T) {
	s := New(Config{})
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	scanned := 0
	for i := 0; i < 24*7; i++ {
		at, kinds := s.nextWake(now)
		if at.IsZero() {
			break
		}
		scanned++
		for _, k := range kinds {
			if k.key() == TaskKeyRunner {
				t.Fatalf("%v 的唤醒点包含 runner——一次性任务不该定时执行", at)
			}
		}
		now = at.Add(time.Second)
	}
	if scanned == 0 {
		t.Fatal("没有扫到任何唤醒点，测试失去意义")
	}
}

func TestRunnerTriggerable(t *testing.T) {
	// 未知 key 必须仍然报错——确认 kindOf 的登记没把别的 key 一起放进来。
	if _, ok := kindOf(string(TaskKeyRunner)); !ok {
		t.Fatal("runner 应能被 kindOf 解析")
	}
	if _, ok := kindOf("no_such_task"); ok {
		t.Fatal("未登记的 key 不应被解析成功")
	}
}
