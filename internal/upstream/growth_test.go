package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)


// TestGrowthTaskRealShapeParsing 2026-10-14 实测：**带 /v2** 的 tasks 端点返回真任务
// 形态（task_code/accept_status/progress/reward_*），这是 accept/claim 认的那套，
// 也是 task_runner.py 与 88lin 用的那份。不带 /v2 的返回另一套「养虾」任务，见下。
func TestGrowthTaskRealShapeParsing(t *testing.T) {
	// 真任务形态（真实上游 2026-10-14 实测响应，字段裁剪自 17 条里的一条）。
	real := []byte(`{"code":0,"data":{"tasks":[
		{"task_code":"create_canvas","title":"体验「无限画布」模式","locked":false,
		 "accept_status":"accepted","progress":{"current":0,"target":1},
		 "reward_credit":300,"reward_energy":5},
		{"task_code":"chat_5","title":"与 AI 对话 5 次","locked":false,
		 "accept_status":"completed","progress":{"current":5,"target":5},
		 "reward_credit":100,"reward_energy":5},
		{"task_code":"Model_chat_GLM5.2","title":"体验「GLM-5.2」模型","locked":false,
		 "accept_status":"claimed","progress":{"current":1,"target":1}}
	]}}`)
	var env struct {
		Data struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(real, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Tasks) != 3 {
		t.Fatalf("tasks=%d want 3", len(env.Data.Tasks))
	}
	if got := env.Data.Tasks[0].TaskID(); got != "create_canvas" {
		t.Errorf("TaskID()=%q want create_canvas（真形态读 task_code）", got)
	}
	if got := env.Data.Tasks[0].State(); got != "accepted" {
		t.Errorf("State()=%q want accepted", got)
	}
	if got := env.Data.Tasks[0].Progress.Target; got != 1 {
		t.Errorf("Progress.Target=%d want 1", got)
	}
	if got := env.Data.Tasks[0].RewardCredit; got != 300 {
		t.Errorf("RewardCredit=%d want 300", got)
	}
	if got := env.Data.Tasks[1].State(); got != "completed" {
		t.Errorf("State()=%q want completed", got)
	}
	if got := env.Data.Tasks[2].State(); got != "claimed" {
		t.Errorf("State()=%q want claimed", got)
	}

	// 「养虾」形态（不带 /v2 的返回）仍要能解析——上游可能改回单一路径。
	shrimp := []byte(`{"code":0,"data":{"tasks":[
		{"task_id":1,"code":"first_chat","title":"完成一次对话","status":"available","level_name":"养虾尝试"}
	]}}`)
	var env2 struct {
		Data struct {
			Tasks []GrowthTask `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(shrimp, &env2); err != nil {
		t.Fatalf("unmarshal shrimp: %v", err)
	}
	if got := env2.Data.Tasks[0].TaskID(); got != "first_chat" {
		t.Errorf("TaskID()=%q want first_chat（养虾形态读 code）", got)
	}
	if got := env2.Data.Tasks[0].State(); got != "not_accepted" {
		t.Errorf("State()=%q want not_accepted（available 归一为可接单）", got)
	}
}

// TestGrowthTasksPathHasV2Prefix 锁住 /tasks 必须带 /v2。
//
// 不带 /v2 的同一路径**也返回 200**，但内容是另一套「养虾」任务（first_chat 等 5 条，
// code/status 字段），与 accept/claim 的命名空间不匹配——照它跑任务段会静默空转
// （2026-10-14 VPS 日志实锤：任务段永远只有「本段跳过」）。这个坑靠状态码测不出来，
// 只能靠路径断言守住。
func TestGrowthTasksPathHasV2Prefix(t *testing.T) {
	if !strings.HasPrefix(growthTasksPath, "/v2/") {
		t.Errorf("growthTasksPath=%q 必须带 /v2 前缀（否则返回另一套任务，领奖全空转）", growthTasksPath)
	}
	if growthTasksAcceptPath != "/activity/growth/tasks/accept" {
		t.Errorf("accept 路径前缀无关（两前缀都认 task_code），沿用既有写法即可，got %q", growthTasksAcceptPath)
	}
}
