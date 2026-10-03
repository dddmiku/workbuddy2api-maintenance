// activity 一次性触发器：只跑一次 RunActivityNow（5 连发 + 领猫联动），部署后验证活跃上报闭环用，不常驻。
//
// 用法（部署后手动触发）：
//
//	# 容器内：先 cp 进去再 exec
//	docker cp activity-run workbuddy2api:/tmp/activity-run
//	docker exec -w /app workbuddy2api /tmp/activity-run
//
//	# 本地：在项目根目录（需 config.json + auths/ + data/）直接 run
//	go run ./cmd/activity
//
// 配置定位：显式 -config > WB2A_CONFIG > config/config.json > ./config.json。
// 统一运行包把配置放在 config/config.json（docker-compose 把 ./config 挂到 /app/config），
// 历史用法在项目根目录放 ./config.json——两个位置都探测，都不存在时回落「默认值 + WB2A_* env」，
// 与 cmd/server 的 loadConfigOrEnv 同口径（2026-10-02 第二轮体检发现：此前硬编码 ./config.json，
// 容器内 -w /app 的文档用法必然 read config 失败）。
//
// 读取配置里的 auth_dir / state_file / schedule / upstream.timeout_seconds，
// 加载 auths 后构建 pool + upstream，调用 scheduler.RunActivityNow 立即执行一次。
//
// schedule 段复用 internal/config 的同一份 Schedule 结构 + 默认值（issue #49）：
// 与 cmd/server 共用，缺省 activity_report_count=5 不再各自复制漂移。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// cfgFile 只取本工具需要的字段；Schedule 段直接用 internal/config.Schedule
// （与 cmd/server 同源），其余段保持精简内联。
type cfgFile struct {
	AuthDir   string          `json:"auth_dir"`
	StateFile string          `json:"state_file"`
	Schedule  config.Schedule `json:"schedule"`
	Upstream  struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
}

// configCandidates 按优先级返回配置文件候选路径。
func configCandidates(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	if env := strings.TrimSpace(os.Getenv("WB2A_CONFIG")); env != "" {
		return []string{env}
	}
	return []string{filepath.Join("config", "config.json"), "config.json"}
}

// loadConfigFile 返回第一个存在的候选文件内容与路径；都不存在时返回 (nil, "", nil)
// 交由调用方走「默认 + env」兜底。只对「文件不存在」兜底，其余错误原样返回
// ——与 cmd/server 的 loadConfigOrEnv 同一口径（用 errors.Is 而非 os.IsNotExist）。
func loadConfigFile(explicit string) ([]byte, string, error) {
	for _, path := range configCandidates(explicit) {
		// #nosec G304 -- 配置文件路径来自命令行参数或本机环境变量，
		// 与 cmd/server 的 loadConfigOrEnv 同一口径（那里同样标注）。
		// 候选列表不含任何请求输入，本程序也不接受网络输入。
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw, path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, path, err
		}
	}
	return nil, "", nil
}

// applyEnv 覆盖 cfgFile 里可由环境变量指定的字段（与 cmd/server 的 applyEnv 同源语义：
// 非空才覆盖，非法整数明确报错而不是静默忽略）。
func applyEnv(c *cfgFile) error {
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("WB2A_TIMEOUT_SECONDS: %q 非法（需为整数）", v)
		}
		c.Upstream.TimeoutSeconds = n
	}
	return nil
}

func main() {
	cfgPath := flag.String("config", "", "配置文件路径（缺省自动探测 config/config.json、./config.json）")
	flag.Parse()

	raw, path, err := loadConfigFile(*cfgPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	// 先置默认值再 Unmarshal：键缺席（或为 null）时字段原样保留默认，
	// 与 cmd/server 的 Load 同路——缺省 activity_report_count=5 而非 Go 零值 0。
	c := cfgFile{Schedule: config.DefaultSchedule()}
	if raw == nil {
		log.Printf("config not found (tried %s), using defaults+env",
			strings.Join(configCandidates(*cfgPath), ", "))
	} else if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatalf("parse config: %v", err)
	} else {
		log.Printf("config loaded: %s", path)
	}
	if err := applyEnv(&c); err != nil {
		log.Fatalf("apply env: %v", err)
	}
	if err := c.Schedule.Normalize(); err != nil {
		log.Fatalf("normalize schedule: %v", err)
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "data/state.json"
	}
	log.Printf("activity count=%d", c.Schedule.ActivityReportCount)

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s)", len(auths))

	p := pool.New(c.StateFile)
	p.SyncToDir(auths)

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            newUpstream(&c),
		CheckinHours:        c.Schedule.CheckinHours,
		TravelHours:         c.Schedule.TravelHours,
		ActivityHours:       c.Schedule.ActivityHours,
		KeepaliveHours:      c.Schedule.KeepaliveHours,
		ActivityReportCount: c.Schedule.ActivityReportCount,
	})
	sch.RunActivityNow()
	log.Printf("activity run complete")
}

// newUpstream 构造上游客户端并显式接线 global realm 路由。
// activity 是 CN 任务中心的上报工具（global 账号已被 scheduler 的 IsGlobal 门控跳过），
// 但 GlobalEnabled=false 会把工具自造的 global 账号请求路由到 CN base（codebuddy.cn）而必然失败。
// 与 cmd/signin、cmd/credit、cmd/trial 同风格：显式接线，避免 Producer 误读成「未适配」。
func newUpstream(c *cfgFile) *upstream.Client {
	up := upstream.New()
	up.GlobalEnabled = true
	if c.Upstream.TimeoutSeconds > 0 {
		up.HTTP.Timeout = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	}
	return up
}
