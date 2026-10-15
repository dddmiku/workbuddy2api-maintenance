// ═══ 更新日志 ═══
// 2026-09-26：注入来源闸门开关与轮转上限，并打一行生效策略日志。
// 2026-09-26：预热模型目录；内嵌面板首次启动失败时网关继续服务，面板后台重试。
// 2026-09-25：启用共享密钥限流与有界请求明细，热交接/信号退出均在在途结束后关闭。
// 2026-09-25：单容器内托管同版管理台及完整更新包，信号退出同样等待长流和最终状态落盘。
// 2026-09-20：向密钥管理接口提供重复推理保护默认值，支持每把密钥单独覆盖。
// 2026-09-19：将重复推理保护的明确开关传入共享HTTP处理器。
// 2026-09-19：不再向 HTTP handler 传入旧输入倍率，用量始终使用上游原值。
// 2026-09-18：请求、排程结束后再统一关闭用量/账号池/Redis；热更新退出同样等待最终落盘。
// 2026-09-18：热更新退出前显式等待会话 GC 停止，避免 os.Exit 绕过 defer 后继续提交过期删除。
// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/panelruntime"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	// 安卓端：目录与首次运行的配置必须在读配置之前准备好（见 BootstrapAndroidLayout）。
	if err := BootstrapAndroidLayout(*cfgPath); err != nil {
		log.Fatalf("prepare android layout: %v", err)
	}

	cfg, err := loadConfigOrEnv(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	layout, err := prepareUnified(cfg, *cfgPath)
	if err != nil {
		log.Fatalf("prepare unified runtime: %v", err)
	}
	var keyStore *apikeys.Store
	if cfg.APIKeysFile != "" {
		keyStore, err = apikeys.Open(cfg.APIKeysFile, cfg.APIKey)
		if err != nil {
			log.Fatalf("load API keys: %v", err)
		}
		if cfg.APIKeysSocket == "" {
			cfg.APIKeysSocket = filepath.Join(filepath.Dir(cfg.APIKeysFile), "api_keys.sock")
		}
	}
	// 用量账本：按调用密钥累计 token。默认与密钥库同目录（usage.json），
	// 显式配 usage_file 可在单密钥模式下也记账。
	var usageStore *usage.Store
	if cfg.UsageFile == "" && cfg.APIKeysFile != "" {
		cfg.UsageFile = filepath.Join(filepath.Dir(cfg.APIKeysFile), "usage.json")
	}
	if cfg.UsageFile != "" {
		usageStore, err = usage.Open(cfg.UsageFile, 0)
		if err != nil {
			log.Fatalf("load usage ledger: %v", err)
		}
		defer usageStore.Close()
		log.Printf("用量账本已启用：%s", cfg.UsageFile)
	}
	var keyLimits *keylimit.Manager
	var requestStore *requestlog.Store
	if keyStore != nil {
		keyLimits, err = keylimit.Open(filepath.Join(filepath.Dir(cfg.APIKeysFile), "key-limits"))
		if err != nil {
			log.Fatalf("load key limits: %v", err)
		}
	}
	if cfg.UsageFile != "" {
		requestStore, err = requestlog.Open(filepath.Join(filepath.Dir(cfg.UsageFile), "requests.jsonl"), requestlog.Options{})
		if err != nil {
			log.Printf("WARN: [请求明细] 存储不可用，累计用量仍正常记录：%v", err)
		}
	}
	// 任务日志落盘：面板的「最近一次日志」原本是纯内存的，容器一重启就没了，
	// 用户看到「明明运行过」却是空页。落一份到数据目录，重启后仍可读。
	// 目录取不到时退回纯内存（SetTaskLogDir 自己兜住，不拦启动）。
	if dir := filepath.Dir(cfg.APIKeysFile); dir != "" && dir != "." {
		scheduler.SetTaskLogDir(filepath.Join(dir, "task-logs"))
	} else if cfg.StateFile != "" {
		scheduler.SetTaskLogDir(filepath.Join(filepath.Dir(cfg.StateFile), "task-logs"))
	}
	var closeOpsOnce sync.Once
	var closeOpsErr error
	closeOps := func() error {
		closeOpsOnce.Do(func() {
			if requestStore != nil {
				closeOpsErr = errors.Join(closeOpsErr, requestStore.Close())
			}
			if keyLimits != nil {
				closeOpsErr = errors.Join(closeOpsErr, keyLimits.Close())
			}
		})
		return closeOpsErr
	}
	defer closeOps()
	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("已加载 %d 个账号（来自 %s）", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	// 来源级限流闸门开关（缺省开启）；关掉后上游限流时继续换号，不按域暂停选号。
	gateEnabled := cfg.Pool.SourceRateGate == nil || *cfg.Pool.SourceRateGate
	p.SetSourceRateGate(gateEnabled)
	rotate := cfg.Pool.MaxRotate
	if rotate <= 0 {
		rotate = server.DefaultMaxRotate
	}
	gateLabel := "开启（上游限流时暂停该域选号）"
	if !gateEnabled {
		gateLabel = "关闭（上游限流时继续换号）"
	}
	slotWaitLabel := cfg.Pool.SlotWait
	if cfg.PoolSlotWaitDur <= 0 {
		slotWaitLabel = "0s（关闭，健康账号全忙时立即回 503）"
	}
	log.Printf("号池策略：单请求最多换号 %d 次；来源级限流闸门%s；在途名额等待上限 %s",
		rotate, gateLabel, slotWaitLabel)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站图片预算：入站放宽后守住发往上游的体积上限（MB → 字节）。
	up.OutboundImageBudgetBytes = cfg.Server.OutboundImageBudgetMB << 20
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		StateDir:            stateDirOf(cfg.StateFile),
		CheckinHours:        cfg.Schedule.CheckinHours,
		TraeCheckHours:      cfg.Schedule.TraeCheckHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		GrowthHours:         cfg.Schedule.GrowthHours,
		BalanceHours:        cfg.Schedule.BalanceHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:  cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TraeCheckDisabled:   !cfg.Schedule.TraeCheckEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:      !cfg.Schedule.SchoolEnabled,
		CatDisabled:         !cfg.Schedule.CatEnabled,
		GrowthDisabled:      !cfg.Schedule.GrowthEnabled,
		BalanceDisabled:     !cfg.Schedule.BalanceEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("WorkBuddy 签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("WorkBuddy 签到已启用：%v 点（cn/global 签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	// Trae 是另一个站点，独立排程、独立开关（见 scheduler 的 traeCheckAll）。
	switch {
	case !cfg.Schedule.TraeCheckEnabled:
		log.Printf("Trae 签到已禁用（schedule.trae_check_enabled=false）")
	default:
		log.Printf("Trae 签到已启用：%v 点（独立站点，上游 9074 限流时当天不再重试）", cfg.Schedule.TraeCheckHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}
	if !cfg.Schedule.GrowthEnabled {
		log.Printf("成长中心已禁用（schedule.growth_enabled=false）")
	} else {
		log.Printf("成长中心已启用：%v 点（原生七段：旅行/任务奖/补登/兑换/抽奖/Buddy盲盒/展示；仅 CN）", cfg.Schedule.GrowthHours)
	}
	// 余额刷新：与签到解耦的纯读任务，是「优先消耗快过期积分」的数据来源。
	// 关掉它会让池里 credits_expiring 停止更新，选号失去快过期偏好。
	if !cfg.Schedule.BalanceEnabled {
		log.Printf("余额刷新已禁用（schedule.balance_enabled=false）——池内积分与快过期分桶将不再更新，" +
			"「优先消耗快过期积分」选号随之失效")
	} else {
		log.Printf("余额刷新已启用：%v 点（纯读查余额，驱动快过期积分优先消耗）", cfg.Schedule.BalanceHours)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backgroundCtx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()

	// 监听套接字：热更新后的新实例从环境变量继承 FD，其余情况正常监听。
	// 用 listener 而不是 ListenAndServe，才能把套接字交给新实例。
	mainLn, inherited, err := hotupdate.Listen(cfg.Listen)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Listen, err)
	}
	defer mainLn.Close()
	if inherited {
		// #nosec G706 -- fd 号来自本进程继承的监听套接字环境变量，不是外部输入
		log.Printf("[热更新] 已从上一个实例接管监听套接字（fd=%s）",
			os.Getenv(hotupdate.EnvListenFD))
	}

	// 管理 socket：热更新后的新实例同样不能重新 bind 旧进程还在服务的路径，
	// 有继承 FD 就用它，否则照常监听（启动时会清掉上次遗留的死 socket 文件）。
	var adminLn net.Listener
	adminPending := false
	if keyStore != nil || layout.enabled {
		if fd, ok := hotupdate.InheritedAdminFD(); ok {
			adminLn, err = hotupdate.ListenerFromFD(fd, "inherited-admin-socket")
			if err != nil {
				log.Fatalf("inherit API key admin socket: %v", err)
			}
			// #nosec G706 -- 同上：管理 socket 的 fd 号
			log.Printf("[热更新] 已从上一个实例接管管理套接字（fd=%s）", os.Getenv(hotupdate.EnvAdminFD))
		} else {
			adminLn, err = apikeys.ListenUnix(cfg.APIKeysSocket)
			switch {
			case err == nil:
			case errors.Is(err, apikeys.ErrSocketBusy):
				// 旧实例（v1.3.1 及更早）不会把管理 socket 交出来，而它要等本实例报告
				// 就绪才开始收尾，没法同步等；改成后台重试，等它关掉监听后接手。
				adminPending = true
				log.Printf("WARN: [密钥管理] 管理套接字被占用，待上一个实例释放 %s 后重新绑定", cfg.APIKeysSocket)
			default:
				log.Fatalf("listen API key admin socket: %v", err)
			}
		}
		if adminLn != nil {
			defer adminLn.Close()
		}
	}

	// 热更新管理器：查版本、下载校验、交接。OnSwitched 触发本进程优雅停机——
	// 新实例已经在同一套接字上 accept，本进程只负责把在途请求跑完。
	handoverDone := make(chan string, 1)
	updateManager := hotupdate.NewManager(hotupdate.Options{
		// Bundle 只对 Linux 部署成立：它的运行目录就是完整运行包解压出来的那个目录，
		// 「解压到目录再整体替换」才说得通。桌面与安卓的数据目录不是运行目录
		// （桌面版程序与数据同处一个文件夹但由启动器管；安卓的程序在 APK 的
		// nativeLibraryDir 里），拿裸二进制当 tar.gz 解包会直接报 gzip 头非法
		// ——2026-10-13 真机实测踩到。
		Bundle:        layout.enabled && hotupdate.BundleSupported(),
		Enabled:       cfg.Update.Enabled,
		Repo:          cfg.Update.Repo,
		Token:         cfg.Update.Token,
		Proxy:         cfg.Update.Proxy,
		Dir:           updateDir(cfg),
		Args:          os.Args[1:],
		Listener:      mainLn,
		AdminListener: adminLn,
		// 安卓上更新的是 APK（应用私有目录不能执行），交给系统安装器；
		// 其他平台返回 nil，走既有的进程交接。
		StageInstaller: hotupdate.StageInstallerForAndroid(updateDir(cfg)),
		OnSwitched: func() {
			select {
			case handoverDone <- "hot update":
			default:
			}
		},
	})

	// 启动时后台探测一次发布仓库可见性：热更新是否可用由「仓库是否开放」决定，
	// 探测放在后台，不阻塞监听；结果随 /update 状态返回给面板。
	if cfg.Update.Enabled {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			switch visibility := updateManager.RefreshVisibility(ctx); visibility {
			case hotupdate.VisibilityPublic:
				log.Printf("INFO: [update] 发布仓库公开，热更新可用（repo=%s）", cfg.Update.Repo)
			case hotupdate.VisibilityPrivate:
				log.Printf("WARN: [update] 发布仓库为私有，匿名读不到 Release；需配置 update.token 才能热更新（repo=%s）", cfg.Update.Repo)
			default:
				log.Printf("WARN: [update] 无法确认发布仓库可见性（repo=%s），热更新暂不可用", cfg.Update.Repo)
			}
		}()
	}

	h := server.NewHandler(server.Config{
		MaxRotate:             cfg.Pool.MaxRotate,
		MaxSoftRotations:      cfg.Pool.MaxSoftRotations,
		RotateOnClientError:   cfg.Pool.RotateOnClientError != nil && *cfg.Pool.RotateOnClientError,
		SlotWait:              &cfg.PoolSlotWaitDur,
		ReasoningLoopGuard:    &cfg.Features.ReasoningLoopGuard,
		ReasoningLoopStopOnly: cfg.Features.ReasoningLoopStopOnly,
		Pool:                  p,
		Upstream:              up,
		APIKey:                cfg.APIKey,
		APIKeys:               keyStore,
		Session:               sessRouter,
		StickyCount:           sessCount,
		RedisMode:             redisMode,
		SoftCooldown:          cfg.SoftRateDur,
		PromptMode:            cfg.Prompt.Mode,
		PromptText:            cfg.PromptText,
		PromptActNote:         server.ActNoteFor(cfg.Prompt.ActNote),
		Update:                updateManager,
		MaxBodyBytes:          int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		Tasks:                 sch,                               // /tasks 端点：排程自省 + 手动触发
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		Usage:         usageStore,
		KeyLimits:     keyLimits,
		Requests:      requestStore,
	})
	// 模型目录预热：出站补齐输出预算依赖 maxOutputTokens，不能等到有人调用 /v1/models。
	go h.WarmModelCatalogs(backgroundCtx)

	// 管理通道 HTTP 服务：正常运行时就绪；兼容路径下等旧实例释放路径后再起。
	// adminServer 由后台 goroutine 赋值、由停机路径读取，用锁保护。
	var adminMu sync.Mutex
	var adminServer *http.Server
	var connections sync.WaitGroup
	connectionState := trackConnections(&connections)
	// 管理接口的 mux 抽成闭包：它既要挂在管理 socket 上（分进程部署），也要
	// **直接在进程内**交给 Go 面板（同进程部署）。
	//
	// 为什么同进程要走进程内分发而不是回环 TCP：Windows 上 Go 的 net.Dial("unix", …)
	// 直接失败（实测 connect: invalid argument），桌面版经 socket 调管理接口一个都调
	// 不通——密钥页、用量页、热更新页会全挂。安卓上同样没有可靠的 Unix socket。
	// 进程内重载：重新读 auths 与 config，原地更新号池。
	//
	// 面板加号 / 删号 / 改任务开关后都走它。**不换进程**是刻意的：Windows 上无法
	// 交接监听套接字（见 internal/hotupdate/fd_windows.go），走交接会让「加一个账号」
	// 在 Windows 上永远失败——2026-10-13 用户实测：扫码登录成功、凭证已落盘，面板
	// 却一直停在「等待扫码…」。原地重载不需要新进程，三端行为一致，而且更快
	// （毫秒级、无中断、不打断在途请求）。
	reloadRuntime := func(ctx context.Context) error {
		fresh, err := auth.LoadDir(cfg.AuthDir)
		if err != nil {
			return fmt.Errorf("重新读取账号失败：%w", err)
		}
		p.SyncToDir(fresh)
		log.Printf("[reload] 已重新加载 %d 个账号（进程内，未重启）", len(fresh))
		return nil
	}

	adminMux := func() *http.ServeMux {
		mux := http.NewServeMux()
		if keyStore != nil {
			mux.Handle("/keys", keyStore.AdminHandler(cfg.Features.ReasoningLoopGuard))
			mux.Handle("/keys/", keyStore.AdminHandler(cfg.Features.ReasoningLoopGuard))
		}
		mux.HandleFunc("POST /runtime/reload", reloadHandler(reloadRuntime, updateManager))
		mux.Handle("/", h.InternalHandler())
		return mux
	}
	serveAdmin := func(listener net.Listener) {
		server := &http.Server{Handler: adminMux(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, ConnState: connectionState}
		adminMu.Lock()
		if backgroundCtx.Err() != nil {
			adminMu.Unlock()
			_ = listener.Close()
			return
		}
		adminServer = server
		adminMu.Unlock()
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("[密钥管理] 管理服务：%v", err)
			}
		}()
		// 库里 0 把密钥是全新部署的正常起点：库文件已建好，用户在管理台创建第一把
		// 即可。日志里点明这一点，免得运维看到 0 以为功能没开。
		//
		// 用 ListChecked 而不是 List：List 把读取失败吞成空列表，于是「密钥库不可读」
		// 会被打成「0 把密钥」，与全新部署无从区分（2026-10-02 第二轮体检发现）。
		if keyStore == nil {
			return
		}
		infos, err := keyStore.ListChecked()
		if err != nil {
			log.Printf("ERROR: 密钥管理已启用，但密钥库无法读取：%v", err)
		} else if len(infos) == 0 {
			log.Printf("密钥管理已启用（0 个密钥），请在管理台创建第一个密钥")
		} else {
			log.Printf("密钥管理已启用（%d 个密钥）", len(infos))
		}
	}
	shutdownAdmin := func(ctx context.Context) error {
		adminMu.Lock()
		server := adminServer
		adminMu.Unlock()
		if server != nil {
			return shutdownHTTPServer(ctx, server)
		}
		return nil
	}
	if adminLn != nil {
		serveAdmin(adminLn)
	} else if adminPending {
		go func() {
			listener, waitErr := apikeys.WaitUnix(cfg.APIKeysSocket, 5*time.Minute, 500*time.Millisecond)
			if waitErr != nil {
				log.Printf("ERROR: [密钥管理] %v", waitErr)
				return
			}
			log.Printf("[密钥管理] 上一个实例退出后已接管管理套接字")
			serveAdmin(listener)
		}()
	}
	backgroundDone := make(chan struct{})
	go func() {
		defer close(backgroundDone)
		sch.Run(backgroundCtx)
	}()

	var panel *panelruntime.Runtime
	var publicHandler http.Handler = h
	if layout.enabled {
		// 面板引擎二选一（见 cmd/server/panel_go.go）：
		//   - Go 引擎：桌面/安卓（没有 CPython 可用），或显式 WB2API_PANEL_GO=1；
		//   - Python 引擎：容器部署的既有形态，未显式切换时行为零变更。
		if useGoPanel() {
			goPanel, panelErr := startGoPanel(layout, cfg, adminMux())
			if panelErr != nil {
				log.Printf("ERROR: Go 版面板启动失败 (%v)；网关 API 继续服务", panelErr)
			} else {
				publicHandler = withPanel(h, goPanel, lanAllowCIDRs(*cfgPath))
			}
		} else {
			// 面板启动失败（凭据文件损坏、目录不可写、Python 启动过慢等）不再拖垂整个网关：
			// /v1 继续服务，面板在后台重试。
			layout.panel.RetryInitialFailure = true
			panel, err = panelruntime.Start(layout.panel)
			if err != nil {
				if panel == nil {
					log.Fatalf("start embedded panel: %v", err)
				}
				log.Printf("ERROR: 内嵌面板启动失败（%v）；API 继续服务，面板会在后台重试", err)
			}
			publicHandler = withPanel(h, panel, lanAllowCIDRs(*cfgPath))
		}
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           publicHandler,
		ConnState:         connectionState,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// 取值大于 MaxBodyMB 在常规带宽下的上传耗时；聊天请求体上限默认 8MB。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	// drainDone 只在"信号停机"路径关闭；热更新路径会直接用约定退出码结束进程。
	drainDone := make(chan struct{})
	go func() {
		reason := "signal"
		select {
		case <-ctx.Done():
		case why := <-handoverDone:
			reason = why
		}
		if reason != "signal" {
			// 热更新：新实例已接管监听，这里只等在途请求收尾；等待上限放宽到能覆盖
			// 长 SSE（上游 idle_timeout 默认 300s + 收尾）。不设上限会在更新时掐断
			// 正在进行的对话。
			log.Printf("[热更新] 退出前等待在途请求跑完（%s）", reason)
		}
		wait := hotupdate.ShutdownTimeout()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		if err := drainRuntime(shutdownCtx, runtimeShutdown{
			stopBackground: stopBackground, backgroundDone: backgroundDone,
			waitConnections: connections.Wait,
			stopSessions: func() {
				if sessRouter != nil {
					sessRouter.StopGC()
				}
			},
			closePool: p.Close, closeStore: store.Close,
			shutdownAdmin: shutdownAdmin,
			shutdownHTTP: func(ctx context.Context) error {
				return shutdownHTTPServer(ctx, srv)
			},
			closeUsage: func() error {
				var err error
				if usageStore != nil {
					err = usageStore.Close()
				}
				return errors.Join(err, closeOps())
			},
		}); err != nil {
			log.Printf("WARN: [服务] 关停时出错：%v", err)
		}
		if panel != nil {
			panel.Close()
		}
		if reason != "signal" {
			// 约定退出码：容器 PID 1 看到它就不再拉起新实例（套接字已在别人手里），
			// 但保持容器存活。
			log.Printf("[热更新] 交接完成，以退出码 %d 结束", hotupdate.ExitHandover)
			os.Exit(hotupdate.ExitHandover)
		}
		close(drainDone)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("workbuddy2api 已开始监听 %s（密钥鉴权=%v）", cfg.Listen, cfg.APIKey != "")
	// 先起 accept 循环再通知就绪：父进程收到通知后会立刻停止接受新连接，
	// 如果这时本进程还没开始 accept，连接会压在队列里直到本进程接管（不影响正确性，
	// 但排队越短越好）。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(mainLn) }()
	// 交接就绪通知：父进程据此确认新实例已开始服务，然后才停旧实例。
	if err := hotupdate.NotifyReady(); err != nil {
		log.Printf("WARN: [热更新] 通知就绪失败：%v", err)
	}
	if err := <-serveErr; err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	// 监听关闭只是收尾的开始：必须等优雅停机真的把在途请求跑完再返回。
	// 提前 return 会让进程以 0 退出，容器 PID 1 会认为可以结束而销毁容器，
	// 正在流式输出的请求连同新实例一起被杀掉（实测过）。
	<-drainDone
	log.Printf("再见")
}
