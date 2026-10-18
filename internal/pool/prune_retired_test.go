// ═══ 更新日志 ═══
// 2026-10-18：按上游模型目录清理「已下线模型」的 11102 台账条目。
//
//	用户指出模型列表应取自上游接口，不该靠逐账号试错发现模型不存在。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPruneRetiredModelCooldowns 目录里没有的模型，其 11102 条目必须被清掉；
// 目录里仍有的、以及真实限流（6004）条目必须保留。
func TestPruneRetiredModelCooldowns(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 已下线的旧名字（实测 CN 台账里的真实取值）与现役模型混在一起。
	p.BlockModelBackoff("u1", "glm-4.6", "11102 model not available")
	p.BlockModelBackoff("u1", "kimi-k2-thinking", "11102 model not available")
	p.BlockModelBackoff("u1", "glm-5.3", "11102 model not available") // 目录里仍有
	p.CooldownSoftForModel("u1", 600e9, time.Time{}, "glm-5.3", "6004 model rate limit")

	keep := map[string]bool{"glm-5.3": true}
	if n := p.PruneRetiredModelCooldowns(keep); n != 2 {
		t.Fatalf("应清理 2 条已下线模型条目，got=%d", n)
	}

	p.mu.RLock()
	mc := p.byUID["u1"].modelCooldowns
	p.mu.RUnlock()
	if _, ok := mc["glm-4.6"]; ok {
		t.Fatal("glm-4.6 不在目录里，条目应被清理")
	}
	if _, ok := mc["kimi-k2-thinking"]; ok {
		t.Fatal("kimi-k2-thinking 不在目录里，条目应被清理")
	}
	row, ok := mc["glm-5.3"]
	if !ok {
		t.Fatal("glm-5.3 在目录里，条目必须保留")
	}
	// 保留的那条是真实限流（6004 覆盖了同名 11102），不能被误清成 11102 语义。
	if row.Reason == "" {
		t.Fatal("保留条目应仍有 reason")
	}
}

// TestPruneRetiredModelCooldownsKeepsRateLimits 6004 限流即便模型名不在目录里
// 也不该被这里清掉——那是真实限流，与「模型是否下线」无关。
func TestPruneRetiredModelCooldownsKeepsRateLimits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600e9, time.Time{}, "some-model", "6004 model rate limit")

	if n := p.PruneRetiredModelCooldowns(map[string]bool{"other": true}); n != 0 {
		t.Fatalf("6004 限流不该被清理，got=%d", n)
	}
	p.mu.RLock()
	_, ok := p.byUID["u1"].modelCooldowns["some-model"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("6004 限流条目必须保留")
	}
}

// TestPruneRetiredModelCooldownsNoopOnEmptyCatalog 目录未就绪时不得清任何东西：
// 空集合不能当作「所有模型都下线了」的证据。
func TestPruneRetiredModelCooldownsNoopOnEmptyCatalog(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "glm-4.6", "11102 model not available")

	if n := p.PruneRetiredModelCooldowns(nil); n != 0 {
		t.Fatalf("目录为空时不该清理，got=%d", n)
	}
	p.mu.RLock()
	_, ok := p.byUID["u1"].modelCooldowns["glm-4.6"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("目录为空时必须保留条目（无法判断是否下线）")
	}
}
