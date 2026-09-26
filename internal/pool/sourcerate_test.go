// ═══ 更新日志 ═══
// 2026-09-23：新增来源级限流闸门回归：三个不同账号在窗口内命中才开闸；
//
//	同一账号反复命中不算；窗口外命中不计入；到期自动放行。
package pool

import (
	"testing"
	"time"
)

func TestSourceRateGateOpensOnDistinctAccounts(t *testing.T) {
	p := New("")

	if p.NoteSourceRateLimit("global", "acct-a") {
		t.Fatal("one account must not open the gate")
	}
	if p.NoteSourceRateLimit("global", "acct-b") {
		t.Fatal("two accounts must not open the gate")
	}
	if !p.NoteSourceRateLimit("global", "acct-c") {
		t.Fatal("three distinct accounts must open the gate")
	}
	limited, remaining := p.SourceRateGate("global")
	if !limited {
		t.Fatal("gate should be open after three distinct accounts")
	}
	if remaining <= 0 || remaining > sourceRateCooldown {
		t.Fatalf("remaining=%v out of range", remaining)
	}
	// 另一个域不受影响：闸门按 realm 隔离。
	if limited, _ := p.SourceRateGate("cn"); limited {
		t.Fatal("cn gate must not be affected by global hits")
	}
	// 另一个池实例不受影响：闸门不跨实例共享（否则测试与多实例场景互相干扰）。
	other := New("")
	if limited, _ := other.SourceRateGate("global"); limited {
		t.Fatal("gate must not leak across Pool instances")
	}
}

func TestSourceRateGateIgnoresRepeatedSameAccount(t *testing.T) {
	p := New("")

	for i := 0; i < sourceRateThreshold*4; i++ {
		if p.NoteSourceRateLimit("global", "same-account") {
			t.Fatal("one account hammering must not open the gate")
		}
	}
	if limited, _ := p.SourceRateGate("global"); limited {
		t.Fatal("gate opened on a single account")
	}
}

func TestSourceRateGateExpiresAndReopens(t *testing.T) {
	p := New("")

	for _, uid := range []string{"a", "b", "c"} {
		p.NoteSourceRateLimit("global", uid)
	}
	limited, _ := p.SourceRateGate("global")
	if !limited {
		t.Fatal("gate should be open")
	}

	// 把闸门到期时间拨到过去，模拟冷却结束：应当放行并清除状态。
	p.mu.Lock()
	p.sourceRateGates["global"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if limited, _ := p.SourceRateGate("global"); limited {
		t.Fatal("expired gate must release")
	}
	// 半开探测再命中：重新累计，仍需要三个不同账号才开闸。
	if p.NoteSourceRateLimit("global", "d") {
		t.Fatal("gate must not reopen on a single new account")
	}
}

// 闸门关闭后：命中不再累计、也不报告暂停 —— 上游限流时继续换号。
func TestSourceRateGateCanBeDisabled(t *testing.T) {
	p := New("")
	defer p.Close()
	p.SetSourceRateGate(false) // 默认开启；这里先关掉验证开关生效
	for _, uid := range []string{"a", "b", "c", "d"} {
		if p.NoteSourceRateLimit("global", uid) {
			t.Fatalf("disabled gate reported a trip on %s", uid)
		}
	}
	if limited, wait := p.SourceRateGate("global"); limited || wait != 0 {
		t.Fatalf("disabled gate is still pausing: limited=%v wait=%s", limited, wait)
	}
	p.SetSourceRateGate(true)
	tripped := false
	for _, uid := range []string{"e", "f", "g"} {
		tripped = tripped || p.NoteSourceRateLimit("global", uid)
	}
	if !tripped {
		t.Fatal("re-enabled gate did not trip after three distinct accounts")
	}
}
