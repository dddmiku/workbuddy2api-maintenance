package upstream

import (
	"encoding/json"
	"testing"
)


// TestGrowthTaskNewShapeParsing 2026-10-14 实测：服务端 tasks 端点已切换到新字段名
// （code/status），不再是 88lin 记录的 task_code/accept_status。两代都要能解析，
// 否则任务段会静默空转。
func TestGrowthTaskNewShapeParsing(t *testing.T) {
	// 新形态（真实上游 2026-10-14 实测响应）。
	newShape := []byte(`{"code":0,"data":{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","status":"available","level_name":"养虾尝试"},
		{"task_id":2,"code":"skill_installed","title":"完成技能安装","status":"completed"}
	]}}`)
	var env struct {
		Data struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(newShape, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Tasks) != 2 {
		t.Fatalf("tasks=%d want 2", len(env.Data.Tasks))
	}
	if got := env.Data.Tasks[0].TaskID(); got != "first_chat" {
		t.Errorf("TaskID()=%q want first_chat（新形态读 code）", got)
	}
	if got := env.Data.Tasks[0].State(); got != "not_accepted" {
		t.Errorf("State()=%q want not_accepted（available 归一为可接单）", got)
	}
	if got := env.Data.Tasks[1].State(); got != "completed" {
		t.Errorf("State()=%q want completed", got)
	}

	// 旧形态（88lin 记录）仍要能解析。
	oldShape := []byte(`{"code":0,"data":{"tasks":[
		{"task_code":"t1","title":"旧任务","locked":false,"accept_status":"not_accepted","reward_credit":10}
	]}}`)
	var env2 struct {
		Data struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(oldShape, &env2); err != nil {
		t.Fatalf("unmarshal old: %v", err)
	}
	if got := env2.Data.Tasks[0].TaskID(); got != "t1" {
		t.Errorf("TaskID()=%q want t1（旧形态读 task_code）", got)
	}
	if got := env2.Data.Tasks[0].State(); got != "not_accepted" {
		t.Errorf("State()=%q want not_accepted", got)
	}
	if got := env2.Data.Tasks[0].RewardCredit; got != 10 {
		t.Errorf("RewardCredit=%d want 10", got)
	}
}
