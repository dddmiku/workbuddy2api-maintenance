package config

import (
	"strings"
	"testing"
)

// TestDefaultScheduleActivityCount 缺省 ActivityReportCount=5（与 cmd/server Default() 对齐）。
// issue #49：cmd/activity 曾因复制结构体无默认值，缺省回落到 1，与 server 的 5 漂移。
func TestDefaultScheduleActivityCount(t *testing.T) {
	s := DefaultSchedule()
	if s.ActivityReportCount != 5 {
		t.Errorf("default ActivityReportCount=%d want 5", s.ActivityReportCount)
	}
	if !s.CheckinEnabled || !s.TravelEnabled || !s.ActivityEnabled || !s.KeepaliveEnabled {
		t.Errorf("all switches must default true: %+v", s)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 || s.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want [9 21]", s.CheckinHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", s.ActivityHours)
	}
	if len(s.SchoolHours) != 1 || s.SchoolHours[0] != 13 {
		t.Errorf("school_hours=%v want [13]", s.SchoolHours)
	}
	if len(s.CatHours) != 1 || s.CatHours[0] != 1 {
		t.Errorf("cat_hours=%v want [1]", s.CatHours)
	}
	if !s.SchoolEnabled || !s.CatEnabled {
		t.Errorf("school/cat switches must default true: %+v", s)
	}
}

// TestNormalizeScheduleThreeStates 缺省/显式 0/显式 N 三态默认值：
//   - 缺省（空 schedule）→ ActivityReportCount 保留 DefaultSchedule 的 5，hours 回落默认。
//   - 显式 0  → 归一为 1（兼容旧行为）。
//   - 显式 N  → 保留 N。
func TestNormalizeScheduleThreeStates(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		want int
	}{
		{"absent_keeps_default", DefaultSchedule(), 5},
		{"explicit_zero_to_one", Schedule{ActivityReportCount: 0}, 1},
		{"explicit_negative_to_one", Schedule{ActivityReportCount: -3}, 1},
		{"explicit_n", Schedule{ActivityReportCount: 8}, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			if err := s.Normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if s.ActivityReportCount != tc.want {
				t.Errorf("ActivityReportCount=%d want %d", s.ActivityReportCount, tc.want)
			}
		})
	}
}

// TestNormalizeScheduleEmptyHoursFallback 空数组/缺省 hours 一律回落默认。
func TestNormalizeScheduleEmptyHoursFallback(t *testing.T) {
	s := Schedule{ActivityReportCount: 5} // hours 全零值（未配）
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.CheckinHours) != 2 || s.CheckinHours[0] != 9 || s.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want [9 21]", s.CheckinHours)
	}
	if len(s.KeepaliveHours) != 1 || s.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want [22]", s.KeepaliveHours)
	}
	if len(s.TravelHours) != 1 || s.TravelHours[0] != 15 {
		t.Errorf("travel_hours=%v want [15]", s.TravelHours)
	}
	if len(s.ActivityHours) != 1 || s.ActivityHours[0] != 10 {
		t.Errorf("activity_hours=%v want [10]", s.ActivityHours)
	}
	if len(s.SchoolHours) != 1 || s.SchoolHours[0] != 13 {
		t.Errorf("school_hours=%v want [13]", s.SchoolHours)
	}
	if len(s.CatHours) != 1 || s.CatHours[0] != 1 {
		t.Errorf("cat_hours=%v want [1]", s.CatHours)
	}
}

// TestNormalizeScheduleInvalidHour 非法小时快速失败并指向正确开关。
func TestNormalizeScheduleInvalidHour(t *testing.T) {
	cases := []struct {
		s          Schedule
		wantSwitch string
	}{
		{Schedule{CheckinHours: []int{25}}, "checkin_enabled"},
		{Schedule{CheckinHours: []int{-1}}, "checkin_enabled"},
		{Schedule{KeepaliveHours: []int{24}}, "keepalive_enabled"},
		{Schedule{TravelHours: []int{-1}}, "travel_enabled"},
		{Schedule{ActivityHours: []int{24}}, "activity_enabled"},
		{Schedule{SchoolHours: []int{25}}, "school_enabled"},
		{Schedule{SchoolHours: []int{-1}}, "school_enabled"},
		{Schedule{CatHours: []int{24}}, "cat_enabled"},
		{Schedule{CatHours: []int{-1}}, "cat_enabled"},
	}
	for _, tc := range cases {
		err := tc.s.Normalize()
		if err == nil {
			t.Errorf("want error for %+v", tc.s)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error %q should point at schedule.%s", err.Error(), tc.wantSwitch)
		}
	}
}

// TestTraeCheckScheduleDefaults Trae 签到是独立排程段：默认 [10]、缺省启用、
// 空值回落、非法小时报错指向 trae_check_enabled。
//
// 2026-10-11：Trae 与 WorkBuddy 拆成两个站点两套排程（用户要求）。
func TestTraeCheckScheduleDefaults(t *testing.T) {
	d := DefaultSchedule()
	if len(d.TraeCheckHours) != 1 || d.TraeCheckHours[0] != 11 {
		t.Fatalf("默认 trae_check_hours 应为 [11]，实际 %v", d.TraeCheckHours)
	}
	if !d.TraeCheckEnabled {
		t.Fatal("默认 trae_check_enabled 应为 true")
	}
	s := Schedule{}
	if err := s.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(s.TraeCheckHours) != 1 || s.TraeCheckHours[0] != 11 {
		t.Fatalf("归一化后应为 [11]，实际 %v", s.TraeCheckHours)
	}
	bad := Schedule{TraeCheckHours: []int{99}}
	err := bad.Normalize()
	if err == nil {
		t.Fatal("非法小时应报错")
	}
	if !strings.Contains(err.Error(), "trae_check_enabled") {
		t.Fatalf("错误信息应指向 trae_check_enabled，实际 %q", err.Error())
	}
}
