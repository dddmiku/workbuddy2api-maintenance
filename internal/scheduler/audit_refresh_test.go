// ═══ 更新日志 ═══
// 2026-09-24：复现签到成功刷新后仍累积旧 12153 计数、误禁用已恢复账号。
package scheduler

import (
	"path/filepath"
	"testing"
)

func TestAuditCheckinRefreshSuccessResetsSessionDeadStreak(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 500}
	s, p := newCheckinS(t, stub)
	a := p.AuthByUID("u1")
	a.Lock()
	a.ExpiresAt = 1
	a.FilePath = filepath.Join(t.TempDir(), "workbuddy-test.json")
	a.Unlock()
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	results, err := s.CheckinAll()
	if err != nil || len(results) != 1 || results[0].Status != CheckinOK || stub.refreshCalls.Load() != 1 {
		t.Fatalf("expected successful checkin and refresh, results=%v err=%v refreshes=%d", results, err, stub.refreshCalls.Load())
	}
	if a.Snapshot().AccessToken != "new" {
		t.Fatal("test did not exercise the successful credential refresh")
	}
	for i := 1; i <= 2; i++ {
		if p.NoteSessionDead("u1") {
			t.Fatalf("account disabled after only %d new failure(s): successful checkin refresh did not clear old failures", i)
		}
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("three subsequent failures must still disable the account")
	}
}
