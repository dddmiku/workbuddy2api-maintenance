package config

import (
	"encoding/json"
	"testing"
)

// TestLegacyScheduleKeysIgnored 旧配置里的 redeem/lottery/makeup 键必须被静默忽略
// （2026-10-14 合并为 growth 后），不能报错、不能影响其它字段。
func TestLegacyScheduleKeysIgnored(t *testing.T) {
	legacy := []byte(`{
		"checkin_hours":[9,21],
		"redeem_hours":[9],
		"lottery_hours":[21],
		"makeup_hours":[9],
		"redeem_enabled":false,
		"lottery_enabled":true,
		"makeup_enabled":true,
		"growth_enabled":true,
		"growth_hours":[8,20]
	}`)
	s := DefaultSchedule()
	if err := json.Unmarshal(legacy, &s); err != nil {
		t.Fatalf("旧配置应能加载（未知键忽略），实际报错: %v", err)
	}
	if err := s.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(s.GrowthHours) != 2 || s.GrowthHours[0] != 8 || s.GrowthHours[1] != 20 {
		t.Errorf("growth_hours=%v want [8 20]", s.GrowthHours)
	}
	if !s.GrowthEnabled {
		t.Error("growth_enabled 应为 true")
	}
	if len(s.CheckinHours) != 2 {
		t.Errorf("checkin_hours 受污染: %v", s.CheckinHours)
	}
}
