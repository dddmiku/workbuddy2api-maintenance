// ═══ 更新日志 ═══
// 2026-09-15: 新增。任务日志归集：把任务执行期间的日志行按任务归到内存环里，
//   供 GET /tasks/{key}/log 与管理面板「任务日志」查看。
//   起因：开学季/夜猫子是子脚本任务，exec.Cmd 默认不接 stdout/stderr，
//   容器日志里只剩一行 "school: ok"——出了问题看不到脚本说了什么。

package scheduler

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// taskLogRing 每类任务保留的日志行数上限（只留最近一次执行的后段，
// 面板看的是"刚才那次跑了什么"，不是完整归档）。
const taskLogRing = 400

// taskLogSink 任务日志环。cur 为当前正在执行的任务 key：为空表示没有任务在跑，
// 此时的日志行一律丢弃——否则运行期日志（HTTP 请求表格等）会把任务日志冲掉。
//
// prev 保留**上一次执行**的行与完成时刻：面板要看的是「最近一次跑了什么」，
// 而执行开始时环会被清空重写，不另存一份的话，点开日志正好赶上新一轮开始
// 就只能看到半截新日志（2026-10-11 用户反馈「看不到最近一次的日志」）。
type taskLogSink struct {
	mu    sync.Mutex
	cur   TaskKey
	ring  map[TaskKey][]string
	prev  map[TaskKey][]string
	ended map[TaskKey]time.Time
	// dir 是落盘目录（空 = 不落盘）。日志环原本是纯内存的，容器一重启
	// 「最近一次日志」就没了——用户看到「明明运行过」却是空页（2026-10-11 实测：
	// 部署重启后 8 分钟，22:44 那次 trae_check 的运行记录查不到）。落一份到磁盘，
	// 重启后仍能看到上次跑了什么。
	dir string
}

var taskSink = &taskLogSink{
	ring:  map[TaskKey][]string{},
	prev:  map[TaskKey][]string{},
	ended: map[TaskKey]time.Time{},
}

// SetTaskLogDir 指定任务日志的落盘目录（空 = 纯内存，行为与引入前一致）。
// 由 main 在装配阶段调用；目录不可写时只记一条日志并退回纯内存，
// 不能让「日志落不了盘」把启动拦下来。
func SetTaskLogDir(dir string) {
	taskSink.mu.Lock()
	taskSink.dir = dir
	taskSink.mu.Unlock()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("WARN: [tasklog] 落盘目录不可用，任务日志将只保留在内存：%v", err)
		taskSink.mu.Lock()
		taskSink.dir = ""
		taskSink.mu.Unlock()
		return
	}
	taskSink.load()
}

// taskLogFile 单个任务的落盘文件（文件名用任务 key，天然合法且可读）。
func taskLogFile(dir string, k TaskKey) string {
	return filepath.Join(dir, string(k)+".json")
}

// taskLogDoc 落盘形态。带版本号便于以后改结构。
type taskLogDoc struct {
	Version int      `json:"version"`
	Lines   []string `json:"lines"`
	EndedAt string   `json:"ended_at,omitempty"`
}

// persistLocked 把某任务的「最近一次」写到磁盘。调用方必须已持 s.mu。
//
// 返回写失败的原因（nil = 成功或无需落盘），由**调用方在释放锁之后**记日志。
// 绝不能在本函数内 log：标准 logger 被 ensureLogTee 分流回 taskSink.Write，
// 那条路径要拿同一把 s.mu —— 持锁打日志会自死锁（磁盘满/目录只读时必然触发，
// 表现为整个调度器卡死）。2026-10-11 由 TestBeginTaskMutualExclusion 挂起暴露。
func (s *taskLogSink) persistLocked(k TaskKey) error {
	if s.dir == "" {
		return nil
	}
	doc := taskLogDoc{Version: 1, Lines: s.prev[k]}
	if t := s.ended[k]; !t.IsZero() {
		doc.EndedAt = t.UTC().Format(time.RFC3339)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	// 原子写：先写临时文件再 rename，避免读到写了一半的文件。
	tmp := taskLogFile(s.dir, k) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, taskLogFile(s.dir, k)); err != nil {
		return err
	}
	return nil
}

// persistAndReport 在锁外记日志的包装：调用方须**不持** s.mu。
func (s *taskLogSink) persistAndReport(k TaskKey) {
	s.mu.Lock()
	err := s.persistLocked(k)
	s.mu.Unlock()
	if err != nil {
		// 日志是观测手段，写失败不能影响任务执行；只提示一次级别的问题。
		log.Printf("WARN: [tasklog] 任务日志落盘失败（%s）：%v", k, err)
	}
}

// load 启动时把落盘的「最近一次」读回内存。
func (s *taskLogSink) load() {
	s.mu.Lock()
	dir := s.dir
	s.mu.Unlock()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var doc taskLogDoc
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Lines) == 0 {
			continue
		}
		k := TaskKey(strings.TrimSuffix(e.Name(), ".json"))
		if k == "" {
			continue
		}
		s.mu.Lock()
		if s.prev == nil {
			s.prev = map[TaskKey][]string{}
		}
		if s.ended == nil {
			s.ended = map[TaskKey]time.Time{}
		}
		s.prev[k] = doc.Lines
		if t, err := time.Parse(time.RFC3339, doc.EndedAt); err == nil {
			s.ended[k] = t
		}
		s.mu.Unlock()
	}
}

// teeWriter 把日志行同时写进基底 writer 与任务日志环。
// 基底先写：即便环这一侧出问题，容器日志也不会因此丢行。
type teeWriter struct {
	base io.Writer
	ring *taskLogSink
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.base.Write(p)
	// 环这一侧是尽力而为的旁路：它写失败不能改变 base 的写入结果，
	// 也不该在这里额外报错（否则每次写日志都会多一条失败噪音）。
	_, _ = t.ring.Write(p)
	return n, err
}

var logTeeMu sync.Mutex

// ensureLogTee 保证标准 logger 的输出分流一份进任务日志环。
//
// 每次任务开始前都调用，而不是只装一次：log.SetOutput 是全局单点，测试与宿主
// 都可能替换它，那条链随时会断——只装一次的写法一旦被替换就永久失效，
// 表现是"面板上任务日志永远是空的"。这里检测到链条不在自己手上就按当前
// writer 重新包一层，既不吞掉别人的输出，也保证归集自持。
func ensureLogTee() {
	logTeeMu.Lock()
	defer logTeeMu.Unlock()
	if t, ok := log.Writer().(*teeWriter); ok && t.ring == taskSink {
		return
	}
	log.SetOutput(&teeWriter{base: log.Writer(), ring: taskSink})
}

// Write 实现 io.Writer：把一行追加到当前任务的环里。
// 没有任务在跑时静默丢弃（返回成功，不干扰 MultiWriter 的其它分支）。
func (s *taskLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == "" {
		return len(p), nil
	}
	line := strings.TrimRight(string(p), "\n")
	if strings.TrimSpace(line) == "" {
		return len(p), nil
	}
	buf := append(s.ring[s.cur], line)
	if len(buf) > taskLogRing {
		buf = buf[len(buf)-taskLogRing:]
	}
	s.ring[s.cur] = buf
	return len(p), nil
}

func (s *taskLogSink) begin(k TaskKey) {
	ensureLogTee()
	s.mu.Lock()
	// 把上一轮的行挪进 prev 再清空当前环：新一轮开始时面板仍能读到「上次跑了什么」。
	if lines := s.ring[k]; len(lines) > 0 {
		s.prev[k] = lines
	}
	s.cur = k
	s.ring[k] = nil // 新一轮执行从干净的一页开始
	s.mu.Unlock()
}

func (s *taskLogSink) end() {
	s.mu.Lock()
	// 结束即把这轮定为「最近一次」：否则 prev 里留的还是更早那一轮，
	// 面板刚跑完一次却显示上一次的内容。
	var toPersist TaskKey
	if s.cur != "" {
		if lines := s.ring[s.cur]; len(lines) > 0 {
			s.prev[s.cur] = lines
		}
		toPersist = s.cur
	}
	s.cur = ""
	s.mu.Unlock()
	if toPersist != "" {
		s.persistAndReport(toPersist)
	}
}

// lastEnded 返回某任务最后一次执行的完成时刻（零值 = 未知）。
// 进程重启后内存为空，但落盘的那份还在——任务页的「上次运行」据此恢复。
func (s *taskLogSink) lastEnded(k TaskKey) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended[k]
}

// markEnded 记下某任务最后一次执行的完成时刻（面板据此显示「最近一次」）。
func (s *taskLogSink) markEnded(k TaskKey) {
	s.mu.Lock()
	if s.ended == nil {
		s.ended = map[TaskKey]time.Time{}
	}
	s.ended[k] = time.Now()
	s.mu.Unlock()
	s.persistAndReport(k)
}

// emit 把子脚本输出的一行按"当前任务"的上下文记进标准 logger，
// 从而同时落到容器日志与任务日志环。无任务上下文时原样记。
func (s *taskLogSink) emit(line string) {
	s.mu.Lock()
	key := s.cur
	s.mu.Unlock()
	if key == "" {
		log.Print(line)
		return
	}
	log.Printf("[%s] %s", key, line)
}

// scriptSink 接管子脚本的 stdout/stderr：按行分流进日志（见 taskLogSink.emit）。
// 不做行缓冲的跨 Write 拼接：脚本输出本就是行缓冲，行尾残留会在下一条 Write 里补齐；
// 极端情况下一条超长行可能被拆成两行，对"看脚本说了什么"这个用途可以接受。
type scriptSink struct{}

func (scriptSink) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		taskSink.emit(strings.TrimRight(line, "\r"))
	}
	return len(p), nil
}

// TaskLogResult 是一次任务日志查询的结果。
type TaskLogResult struct {
	// Lines 日志行（旧 → 新）。任务在跑时是**当前**这一轮；空闲时是**上一次**那一轮。
	Lines []string
	// Running 该任务此刻是否正在执行（面板据此显示「运行中」并加快轮询）。
	Running bool
	// StartedAt / EndedAt 本次（或上一次）执行的起止时刻；未知为零值。
	StartedAt time.Time
	EndedAt   time.Time
}

// TaskLog 返回某任务最近一次执行的日志行（旧 → 新）。
//
// 任务正在执行时返回**当前**这一轮（实时刷新要看的就是它）；空闲时返回
// **上一次**那一轮（面板的「最近一次日志」）。两者都不存在时返回空切片。
// 未知 key 返回 ErrUnknownTask。
func (s *Scheduler) TaskLog(key string) ([]string, error) {
	res, err := s.TaskLogDetail(key)
	if err != nil {
		return nil, err
	}
	return res.Lines, nil
}

// TaskLogDetail 同 TaskLog，但额外给出运行状态与起止时刻。
func (s *Scheduler) TaskLogDetail(key string) (TaskLogResult, error) {
	k, ok := kindOf(key)
	if !ok {
		return TaskLogResult{}, ErrUnknownTask
	}
	tk := k.key()
	taskSink.mu.Lock()
	defer taskSink.mu.Unlock()
	// 正在跑：当前环是活的，直接给（含它这一轮已产出的全部行）。
	// 空闲：给上一轮留下的 prev；没有 prev 时退回当前环（老进程刚升级上来时
	// 只有 ring 有内容，不能因为没 prev 就报空）。
	lines := taskSink.ring[tk]
	running := taskSink.cur == tk
	if !running && len(taskSink.prev[tk]) > 0 {
		lines = taskSink.prev[tk]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	res := TaskLogResult{Lines: out, Running: running, EndedAt: taskSink.ended[tk]}
	// 起止时刻取任务自省表（beginTask/endTask 记的那份，与面板「上次运行」同源）。
	s.taskMu.Lock()
	if t, ok := s.taskLast[tk]; ok {
		res.EndedAt = t
	}
	s.taskMu.Unlock()
	if running {
		res.StartedAt = time.Now() // 精确开始时刻不可得时给当前时刻，仅用于「运行中」展示
	}
	return res, nil
}
