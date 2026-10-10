package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// 2026-10-10：付费 vs 免费的对照实验。线上排查时我一度以为过滤"过度生效"——
// 免费模型 cn:hy3 也只落在那 5 个快过期号上。查下来是环境造成的假象：池里
// 健康 CN 号本来就只有 5 个（28 个号里 23 个是 global、7 个被禁用），
// 两个模型当然都只可能落在那 5 个上。
// 本用例在可控环境里做真对照：8 个号、其中 2 个有快过期积分。
//   - 付费模型：只从这 2 个里选（硬过滤）
//   - 免费模型：8 个都可能被选到（不受过滤）
func TestPaidVsFreeOnSamePool(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"e1", "e2", "n1", "n2", "n3", "n4", "n5", "n6"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetCredits("e1", 1000)
	p.SetCredits("e2", 1000)
	for _, uid := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		p.SetCredits(uid, 1000)
	}
	// 只有 e1/e2 有快过期积分。
	p.SetCreditsDetailed("e1", 1000, 800)
	p.SetCreditsDetailed("e2", 1000, 400)
	p.SetPaidModels(map[string]bool{"paid": true})

	paidSeen := map[string]int{}
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealm(nil, "paid", "")
		if got == nil {
			t.Fatal("paid pick nil")
		}
		paidSeen[got.UID]++
	}
	for uid := range paidSeen {
		if uid != "e1" && uid != "e2" {
			t.Errorf("付费模型选中了无快过期积分的 %s（seen=%v）", uid, paidSeen)
		}
	}
	if len(paidSeen) != 2 {
		t.Errorf("付费模型应只在 e1/e2 之间选，seen=%v", paidSeen)
	}

	freeSeen := map[string]int{}
	for i := 0; i < 400; i++ {
		got := p.PickExcludingForRealm(nil, "free", "")
		if got == nil {
			t.Fatal("free pick nil")
		}
		freeSeen[got.UID]++
	}
	if len(freeSeen) < 6 {
		t.Errorf("免费模型应分散在多数号上（8 个号），seen=%v", freeSeen)
	}
}
