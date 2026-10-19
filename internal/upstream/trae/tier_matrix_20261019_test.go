package trae

import (
	"os"
	"testing"
)

// 用 2026-10-19 抓的真实目录锁住档位矩阵。
//
// 实测（真实上游，2026-10-19）：
//
//	solo_work_lite + __dev → 200
//	solo_work_lite + __max → 4023 "the model is unknown"
//	solo_agent     + __dev → 200
//	solo_agent     + __max → 200
//
// 即 __max 只存在于 solo_agent / solo_agent_remote / solo_agent_lite / chat_v3
// 四张表；solo_work_lite / solo_work_remote 只有 __dev。
//
// functionPreference 把 solo_work_lite 排在第一位，所以 FunctionForModelTier
// 在 wantMax=true 时必须跳过它去找 solo_agent —— 否则 1M 档永远发不出去
// （这正是 c7b8253 修掉的「恒发 200K」）。
func TestTierMatrixFromRealCatalog(t *testing.T) {
	data, err := os.ReadFile("testdata/catalog_real_20261019.json")
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	models, err := ParseCatalog(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	m, ok := LookupCatalogModel(models, "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("deepseek-v4.1-flash absent")
	}

	// 1) 无 max 档的表必须被识别出来
	for _, fn := range []string{"solo_work_lite", "solo_work_remote"} {
		p, ok := m.Profiles[fn]
		if !ok {
			t.Errorf("%s: profile missing", fn)
			continue
		}
		if p.MaxModelName != "" {
			t.Errorf("%s: expected no __max, got %q", fn, p.MaxModelName)
		}
		if p.DevModelName == "" {
			t.Errorf("%s: expected __dev present", fn)
		}
	}

	// 2) 有 max 档的表必须记下真名
	for _, fn := range []string{"solo_agent", "solo_agent_remote",
		"solo_agent_lite", "chat_v3"} {
		p, ok := m.Profiles[fn]
		if !ok {
			t.Errorf("%s: profile missing", fn)
			continue
		}
		if p.MaxModelName != "deepseek-v4.1-flash__max" {
			t.Errorf("%s: max name = %q, want deepseek-v4.1-flash__max",
				fn, p.MaxModelName)
		}
	}

	// 3) wantMax 时必须切到有 max 档的表
	fn := FunctionForModelTier(models, "deepseek-v4.1-flash", "solo_work_lite", true)
	if fn == "solo_work_lite" || fn == "solo_work_remote" {
		t.Fatalf("wantMax picked a table without __max: %q", fn)
	}
	if got := m.OutboundModelNameFor(fn, true); got != "deepseek-v4.1-flash__max" {
		t.Fatalf("outbound = %q, want deepseek-v4.1-flash__max", got)
	}

	// 4) 不要 max 时保留 preferred（solo_work_lite 有 __dev，可用）
	fnDev := FunctionForModelTier(models, "deepseek-v4.1-flash", "solo_work_lite", false)
	if got := m.OutboundModelNameFor(fnDev, false); got != "deepseek-v4.1-flash__dev" {
		t.Fatalf("dev outbound = %q, want deepseek-v4.1-flash__dev", got)
	}
}
