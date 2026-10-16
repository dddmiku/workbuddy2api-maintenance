// ═══ 更新日志 ═══
// 2026-09-15: 新增。排程任务的 HTTP 面：GET /tasks 返回任务快照，
//   POST /tasks/{key}/run 手动触发。供账户管理面板的「定时任务」页使用。

package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/scheduler"
)

// TaskController 排程任务的自省与手动触发（由 scheduler.Scheduler 实现）。
// 用窄接口而不是直接持有 *scheduler.Scheduler：server 只用到这两个能力，
// 测试可以塞假实现，不必把整个排程器搭起来。
type TaskController interface {
	TaskSnapshot() []scheduler.TaskInfo
	TriggerTask(key string) error
	// TaskLogDetail 返回某类任务最近一次执行的日志行（旧 → 新）与运行状态。
	// 正在执行时给**当前**这一轮（实时刷新看的就是它），空闲时给**上一次**那一轮。
	// 未知 key 返回 scheduler.ErrUnknownTask；跑过但无输出返回空切片。
	TaskLogDetail(key string) (scheduler.TaskLogResult, error)
	// SetTaskEnabled 原地改一个任务的启停，让面板改完 config.json 后**不用重启**
	// 网关就生效（2026-10-15 用户反馈「任务启停都要重启，这合理吗」）。
	// 返回 false 表示 key 未知（此时不写任何状态）。
	SetTaskEnabled(key string, enabled bool) bool
	// StopTask 取消正在跑的某个任务（面板「停止」按钮）。返回 false 表示该任务
	// 当前没在运行（不是错误）。取消是协作式的：返回 true 只表示信号已发出。
	StopTask(key string) bool
}

// tasks 返回全部排程任务的快照。
// 未接入排程器时返回 available=false 而不是 404：面板据此渲染"本进程无排程"的说明态，
// 比一个错误码对使用者更有用。
func (h *Handler) tasks(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"available": false, "tasks": []any{}})
		return
	}
	snap := h.cfg.Tasks.TaskSnapshot()
	if snap == nil {
		snap = []scheduler.TaskInfo{}
	}
	_ = writeJSON(w, http.StatusOK, map[string]any{"available": true, "tasks": snap})
}

// taskLog 返回某类任务最近一次执行的日志行。
// 与 /tasks 分开是刻意的：日志动辄几百行，快照要轮询刷新，不能背着这份体积。
func (h *Handler) taskLog(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法读取任务日志")
		return
	}
	key := r.PathValue("key")
	res, err := h.cfg.Tasks.TaskLogDetail(key)
	if err != nil {
		status := http.StatusBadRequest
		msg := err.Error()
		if errors.Is(err, scheduler.ErrUnknownTask) {
			msg = "未知任务：" + key
		}
		writeOpenAIError(w, status, "task_log_failed", msg)
		return
	}
	// running 让面板区分「正在跑（日志还在长）」与「已结束（这是最近一次）」，
	// 并据此决定要不要加快轮询。ended_at 给「最近一次」一个可读时刻。
	payload := map[string]any{
		"key": key, "lines": res.Lines, "count": len(res.Lines), "running": res.Running,
	}
	if !res.EndedAt.IsZero() {
		payload["ended_at"] = res.EndedAt.UTC().Format(time.RFC3339)
	}
	_ = writeJSON(w, http.StatusOK, payload)
}

// taskRun 手动触发单类任务。立即返回 202，任务在网关侧后台执行；结果落容器日志。
// 不做同步等待：签到/旅行这类要遍历全部账号打上游，耗时以十秒计，
// 让管理面板的 HTTP 请求挂在那里等既超时又占连接。
func (h *Handler) taskRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法手动触发任务")
		return
	}
	key := r.PathValue("key")
	if err := h.cfg.Tasks.TriggerTask(key); err != nil {
		status := http.StatusBadRequest
		msg := err.Error()
		switch {
		case errors.Is(err, scheduler.ErrTaskBusy):
			status = http.StatusConflict
			msg = "该任务正在执行中，等它跑完再试"
		case errors.Is(err, scheduler.ErrUnknownTask):
			msg = "未知任务：" + key
		}
		writeOpenAIError(w, status, "task_trigger_failed", msg)
		return
	}
	_ = writeJSON(w, http.StatusAccepted, map[string]any{
		// 前端收到 ok 后会直接打开该任务的日志窗口（2026-10-15 起），文案不再把
		// 用户引去翻容器日志——日志弹窗就是为看「正在跑什么」准备的。
		"ok": true, "key": key, "message": "已触发",
	})
}

// taskStop 停止正在跑的某个任务（面板「停止」按钮）。
//
// 与 taskRun 对称，但语义不同：停止是**协作式取消**——发出取消信号后任务在下一个
// ctx 检查点退出，因此返回 accepted 只表示"信号已发出"，不代表已停。面板应继续
// 轮询 /tasks 的 running 字段确认（它会变成 false）。
//
// 任务没在跑时返回 200 + stopped=false，而不是错误：面板可能连点，或任务刚好
// 自己结束了——那两种情况用户的意图都已满足（任务不在跑），报错只会让人困惑。
func (h *Handler) taskStop(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法停止任务")
		return
	}
	key := r.PathValue("key")
	stopped := h.cfg.Tasks.StopTask(key)
	message := "已发出停止信号，任务将在当前步骤结束后退出"
	if !stopped {
		message = "该任务当前没有在运行"
	}
	_ = writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "key": key, "stopped": stopped, "message": message,
	})
}

// taskToggle 原地切换一类任务的启停，**不重启进程**。
//
// 为什么要有这个端点：面板改 config.json 里的 schedule.*_enabled 只写了文件，而排程器
// 在启动时把开关抄进了自己的 Config——改完不重启，运行中的排程器仍按旧值派发。
// 用户 2026-10-15 反馈「任务启停都要重启网关才行，这合理吗」。这里让面板改完文件后
// 顺手调一次，网关原地改运行期覆盖，下一次 nextWake 就按新值算时点。
//
// 只走本机管理通道（requireInternal）：它改变的是排程行为，不能让任一调用密钥自己改。
// 请求体 {"key":"checkin","enabled":false}。
func (h *Handler) taskToggle(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "tasks_unavailable",
			"本进程未接入排程器，无法改任务开关")
		return
	}
	var body struct {
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON")
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" || body.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "需要 key 与 enabled")
		return
	}
	if !h.cfg.Tasks.SetTaskEnabled(key, *body.Enabled) {
		writeOpenAIError(w, http.StatusBadRequest, "task_toggle_failed", "未知任务："+key)
		return
	}
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "key": key, "enabled": *body.Enabled,
		"message": "已原地生效，无需重启",
	})
}
