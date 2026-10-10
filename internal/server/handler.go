// ═══ 更新日志 ═══
// 2026-09-28：接受 Anthropic 的 `[1m]` 模型后缀（去后缀后按同一模型路由）。
// 2026-09-28：未知 4xx 换号可选（pool.rotate_on_client_error，默认保持旧契约）。
// 2026-09-28：换号重试上限可配（pool.max_soft_rotations）。
// 2026-09-28：内容审核与未知 4xx 可按配置先换号重试（pool.max_soft_rotations，默认 0 = 旧行为）；重试时解绑会话粘性才能真正换到另一个号。
// 2026-09-28：新增管理通道复活账号入口 POST /accounts/revive。
// 2026-09-28：11140 账号故障改为连续计数后才禁用。
// 2026-09-26：导出 DefaultMaxRotate，供启动日志与配置对齐。
// 2026-09-26：/update/apply 在未启用时如实拒绝（此前谎报已开始）。
// 2026-09-26：压制期回调接线（HoldProgress）；已开流时的失败走流内交付。
// 2026-09-26：错误信封 type 按状态码映射；n<1 拒绝。
// 2026-09-26：拒绝 n>1（上游只返回单个选择），避免静默降级。
// 2026-09-26：上游请求发出时通知响应适配器（供静默期先开流与 ping）。
// 2026-09-26：请求模型名限长 256 字节，防止超长名字撑爆用量账本写盘上限。
// 2026-09-26：参数错误、WAF、渠道与内容拦截等请求决定的终态不再解绑会话，保留同号提示缓存。
// 2026-09-26：会话键交给出站层并记录每轮上游实报输入，用于长会话主动收缩输出预算。
// 2026-09-25：四协议统一密钥限流与消费明细，管理接口只对内部通道开放。
// 2026-09-25：每次实际尝试独立记账和归因，循环重发失败返回当前错误，工具契约拒绝不误报上游成功。
// 2026-09-25：接入Gemini共享鉴权/调度，并在记账前收尾全部输出适配器，防止最终写失败被记为成功。
// 2026-09-25：补齐模型详情与双协议发现鉴权，未知上下文不填假值；统一限制慢客户端写出并告知兼容过滤。
// 2026-09-25：超限保持历史及会话绑定，成本统计统一读取上游实际用量，不再给客户端补写未处理的token。
// 2026-09-25：流内超限与循环重试后的超限保持真实错误信号，不解绑会话、不覆盖已观测用量。
// 2026-09-24：多密钥模式隔离账号与排程管理端点，调用密钥不再具备全局管理权限。
// 2026-09-22：上游代理层 HTML 授权页（APISIX/openresty）不再被当作「请求参数被拒」
// 回显整段 HTML；改为 503 可重试语义、不罚账号、不解绑会话粘性。
// 2026-09-22：密钥模型绑定改为「完整模型名逐字相等」，不再按 resolveModel 解析后比较；
// 此前裸名绑定会被静默扩成 cn: 域的两个名字，实际可用范围大于管理员写下的那一条。
// 2026-09-22：重复推理保护改为「命中后重发 / 命中即停止」可在管理台热切换；开关值
// 存进 handler 的原子字段，改完立即作用于后续请求，不需要重启，在途请求不受影响。
// 2026-09-20：无会话标识的客户端（narrafork 形态）按正文派生对话级回退键补上会话粘性；此前这类请求每轮换号、上游提示缓存整段失效。
// 2026-09-20：已鉴权密钥可覆盖重复推理保护默认值，每次请求使用独立策略快照。
// 2026-09-19：会话绑定和上游关联头按已鉴权密钥隔离，避免不同调用方共用或互相解除绑定。
// 2026-09-19：重复推理保护返回明确非重试错误，停止本次流但保留已知用量和账号/粘性状态。
// 2026-09-19：移除输入倍率依赖，HTTP 出口保留上游原始用量。
// 2026-09-17：合并模型级避让与完整响应校验，仅在确认成功后解除模型负缓存。
// 2026-09-17：密钥可绑定模型白名单，超出范围的请求在选号前拒绝。
// 2026-09-16：保留调用者指令，停止全局自动降级；校验输入并按真实流结果记录成功。
// 2026-09-17：热更新触发不再要求先手动检查远端版本（没查过时由 Apply 自己查）。
// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
// 2026-09-18：直接 Chat 的工具选择与结构化输出复用严格契约，在校验通过前不发布成功终态。
// 2026-09-18：直接 Chat 与 Responses 一致过滤未实现的内置声明，保持无工具请求的上游兼容性。
// 2026-09-18：Chat别名只用于上游传输，验证后恢复公开工具名再交付客户端。
// 2026-09-19：已发起上游的请求统一收尾记账，保留失败/取消用量并标记未完整上报，终态写失败不再漏计。
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/apikeys"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/hotupdate"
	"workbuddy2api/internal/jsonutil"
	"workbuddy2api/internal/keylimit"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/requestlog"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/usage"
	"workbuddy2api/internal/version"
)

// maxReasoningLoopRetries 单请求内「重复推理循环 + 客户端零字节」的同账号重发上限。
// 循环是上游模型行为，重发一次通常就能拿到干净的一轮；持续循环时必须收手并如实
// 回报错误，不能无限重试。
const maxReasoningLoopRetries = 1

// maxReadRotations 上游掐流（且客户端未收到任何内容）时换号重试的上限。
// 取 2：偶发故障换一次通常就好；连换两次仍失败说明上游整体不稳，
// 继续重试只会拖长客户端等待，不如如实报错。
const maxReadRotations = 2

// 换号重试上限由 config 的 pool.max_soft_rotations 决定（0/未配置 = 保持既有契约：
// 内容审核与未知 4xx 直接回给调用方，不换号）。上游的内容审核可能是内容维度而非账号
// 维度，换号未必能过、却会拖慢失败——所以默认关闭，由部署方按需打开。

// Config handler 依赖。
// DefaultMaxRotate 单请求默认最多换号次数：一次客户端请求最多消耗几个账号。
const DefaultMaxRotate = 3

// DefaultSlotWait 「健康账号全被在途名额占满」时的默认等待上限。
//
// 取值依据 2026-10-07 实测：池里剩 2 个健康号、每个 3 个在途名额（共 6 个），
// 客户端并发峰值把它们全占满时，200 与 503 在同一秒内交替出现——名额是几百毫秒
// 到几秒就释放的。等待窗口取 10s 覆盖数个名额周转周期；再长则客户端自身的超时
// 风险开始超过收益，而且等待失败的请求会挤占连接。
const DefaultSlotWait = 10 * time.Second

type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string         // 空 = 不鉴权
	APIKeys   *apikeys.Store // 配置后以持久化密钥库为准，空库不放行。
	MaxRotate int            // 单请求最多换号次数，默认 DefaultMaxRotate
	// MaxSoftRotations 「内容审核 / 未知 4xx」在回给调用方前的换号次数上限，
	// 默认 DefaultMaxSoftRotations（负数 = 0，即不换号，维持旧行为）。
	MaxSoftRotations int
	// SlotWait 「健康账号全被在途名额占满」时，本请求等待名额释放的上限。
	// nil = 用 DefaultSlotWait；显式 0 = 不等待（立即 503，旧行为）。
	// 用指针而不是裸 duration：需要区分「未配置」与「显式关闭」，零值 duration
	// 两者都是 0。风格同本结构体的 ReasoningLoopGuard / RotateOnClientError。
	// 只影响这一类失败：池里根本没有健康账号时不等待（见 slotWaitable）。
	SlotWait *time.Duration
	// RotateOnClientError 未知 4xx 是否也换号再试（默认 false，保持既有契约：
	// 未知 4xx 通常由请求本身决定，换号会掩盖真实错误、放大无效请求）。
	RotateOnClientError bool
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// ReasoningLoopGuard is the server default (nil = enabled). An authenticated
	// key can override it without changing the requested model or reasoning effort.
	ReasoningLoopGuard *bool
	// ReasoningLoopStopOnly 命中重复短行时只停止该次请求，不做同账号重发。
	// 缺省 false = 命中后先在同一账号上重发一次（用户侧无感）；显式 true = 命中即
	// 停止并如实回报，把是否重试交回调用方。只影响「命中之后怎么办」，不影响检测本身。
	// 这是启动默认值；管理台可以在运行期改（见 stopOnly 字段），改完立即生效。
	ReasoningLoopStopOnly bool
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// PromptActNote 运行约定文本，追加在带工具请求的 system 末尾（见 prompt.ActNote）。
	// 空 = 不追加（调用方用 ActNoteFor 解析配置后再传入）。
	PromptActNote string

	// Tasks 排程任务控制器（可选；nil = /tasks 报 available=false，面板渲染说明态）。
	Tasks TaskController

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 按调用密钥累计的 token 账本（可选；nil = /usage 报未启用）。
	// 已发起上游的请求独立记账；已知用量保留，失败和未完整上报另作标记。
	Usage     *usage.Store
	KeyLimits *keylimit.Manager
	Requests  *requestlog.Store

	// Update 热更新管理器（可选；nil = /update/* 报未启用）。
	Update *hotupdate.Manager

	// RegionRepair 补交 global 账号注册地（14017 trial-not-activated 的自愈动作，
	// 见 internal/upstream/region.go）。nil = 用 Upstream.CompleteRegion。
	// 供测试注入确定性实现；生产留空走真实上游。
	RegionRepair func(*auth.Auth) (upstream.UserArea, error)
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// stopOnly 是 ReasoningLoopStopOnly 的运行期值（0 = 关闭，1 = 打开）。
	// 用原子量而不是改 cfg：管理台热切换要立即作用于新请求，同时又不能让已经
	// 开始的重发循环读到半个状态。启动时由 Config 播种，之后只由管理接口写。
	stopOnly atomic.Uint32
	// slotWait 是 Config.SlotWait 解析后的值（nil → DefaultSlotWait）：
	// 选号热路径每轮都要读，避免每次解引用指针 + 判空。
	slotWait         time.Duration
	requestLogErrors atomic.Uint64
}

// SetReasoningLoopStopOnly 运行期切换「命中循环后是否只停不重发」，立即作用于后续
// 请求；已经开始的那次请求沿用开始时读到的值。
func (h *Handler) SetReasoningLoopStopOnly(enabled bool) {
	if enabled {
		h.stopOnly.Store(1)
		return
	}
	h.stopOnly.Store(0)
}

// reasoningLoopStopOnly 返回当前运行期值。Config 里显式打开时同样返回 true。
func (h *Handler) reasoningLoopStopOnly() bool {
	return h.stopOnly.Load() == 1
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = DefaultMaxRotate
	}
	if cfg.MaxSoftRotations < 0 {
		cfg.MaxSoftRotations = 0
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：透传客户端原始 system
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	if cfg.KeyLimits == nil {
		cfg.KeyLimits = keylimit.NewMemory()
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	// 解析等名额上限：nil = 未配置 → 用默认；显式 0 = 关闭等待。
	h.slotWait = DefaultSlotWait
	if cfg.SlotWait != nil {
		h.slotWait = *cfg.SlotWait
	}
	if h.slotWait < 0 {
		h.slotWait = 0
	}
	// 把启动配置播种进运行期开关；之后管理台可以热切换，不必重启。
	h.SetReasoningLoopStopOnly(cfg.ReasoningLoopStopOnly)
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.withGeneration(requestlog.ProtocolChat, h.withDecodedRequest(h.chatCompletions))))
	// Responses API 兼容层（NarraFork / Codex 等客户端走这条）：内部委托 chatCompletions。
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.withGeneration(requestlog.ProtocolResponses, h.withDecodedRequest(h.responses))))
	h.mux.HandleFunc("POST /v1/messages", h.messagesEntry)
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.messagesEntry)
	h.mux.HandleFunc("POST /v1beta/models/{modelAction}", h.withGeminiProtocol(h.geminiContent, true))
	h.mux.HandleFunc("POST /v1/models/{modelAction}", h.withGeminiProtocol(h.geminiContent, true))
	h.mux.HandleFunc("GET /v1beta/models", h.withGeminiProtocol(h.geminiModels, false))
	h.mux.HandleFunc("GET /v1beta/models/{model}", h.withGeminiProtocol(h.geminiModels, false))
	h.mux.HandleFunc("POST /v1beta/interactions", h.withGeminiProtocol(h.unsupportedGeminiTransport, false))
	h.mux.HandleFunc("POST /v1/interactions", h.withGeminiProtocol(h.unsupportedGeminiTransport, false))
	h.mux.HandleFunc("GET /v1/models", h.modelProtocolDiscovery(h.models))
	// 管理台「刷新模型」：强制绕过 1h 缓存回源拉一次（仅本机管理通道可达）。
	h.mux.HandleFunc("POST /models/refresh", h.requireInternal(h.modelsRefresh))
	h.mux.HandleFunc("GET /v1/models/{model}", h.modelProtocolDiscovery(h.model))
	h.mux.HandleFunc("GET /v1/capabilities", h.withDiscoveryAuth(h.capabilities))
	h.mux.HandleFunc("GET /status", h.withAccountAdmin(h.status))
	// 排程任务自省与手动触发（账户管理面板的「定时任务」页）。
	h.mux.HandleFunc("GET /tasks", h.withAccountAdmin(h.tasks))
	h.mux.HandleFunc("POST /tasks/{key}/run", h.withAccountAdmin(h.taskRun))
	h.mux.HandleFunc("GET /tasks/{key}/log", h.withAccountAdmin(h.taskLog))
	// 用量统计只走本机 Unix socket（管理台「用量统计」页）：普通调用密钥拿不到全量用量，
	// 单密钥自己的用量在日志与面板里按 key 归属，不需要公开端点。
	h.mux.HandleFunc("GET /usage", h.requireInternal(h.usageStats))
	h.mux.HandleFunc("GET /key-limits", h.requireInternal(h.keyLimitStatus))
	h.mux.HandleFunc("GET /requests", h.requireInternal(h.requestHistory))
	// 筛选项必须注册在 /requests/{requestID} **之前**：Go 的 mux 按最具体模式优先，
	// 但显式排序让意图明确，也避免将来改成前缀匹配时把 facets 当成请求 ID。
	h.mux.HandleFunc("GET /requests/facets", h.requireInternal(h.requestFacets))
	h.mux.HandleFunc("GET /requests/{requestID}", h.requireInternal(h.requestDetail))
	// 热更新同样只走本机管理通道：能触发版本切换的入口不能暴露给调用密钥。
	h.mux.HandleFunc("GET /update", h.requireInternal(h.updateStatus))
	h.mux.HandleFunc("POST /update/check", h.requireInternal(h.updateCheck))
	h.mux.HandleFunc("POST /update/apply", h.requireInternal(h.updateApply))
	h.mux.HandleFunc("POST /accounts/revive", h.requireInternal(h.reviveAccount))
	// 重复推理保护的「命中后怎么办」热切换，同样只走本机管理通道：它改变的是所有
	// 调用方看到的行为，不能让任一调用密钥自己改。
	h.mux.HandleFunc("GET /features/reasoning-loop", h.requireInternal(h.reasoningLoopFeature))
	h.mux.HandleFunc("POST /features/reasoning-loop", h.requireInternal(h.setReasoningLoopFeature))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = identifyRequest(w, r)
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	trace := &requestTrace{record: requestlog.Record{RequestID: requestID(r), StartedAt: time.Now()}}
	wire := &traceResponseWriter{ResponseWriter: w, trace: trace}
	ctx = context.WithValue(ctx, requestTraceKey{}, trace)
	defer h.finishTrace(trace, wire, ctx)
	h.mux.ServeHTTP(newBoundedResponseWriter(wire, cancel, downstreamWriteTimeout), r.WithContext(ctx))
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(internalAdminContextKey{}) == true {
			next(w, r)
			return
		}
		if h.cfg.APIKeys != nil {
			authz := r.Header.Get("Authorization")
			provided := strings.TrimPrefix(authz, "Bearer ")
			info, status := h.cfg.APIKeys.Lookup(provided)
			if !strings.HasPrefix(authz, "Bearer ") {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			if status == apikeys.StatusExpired {
				// 有效期已过：明确告知原因，否则调用方只会看到「密钥无效」而无从判断。
				// 只有持有正确密钥的人才会走到这里，不构成枚举信号。
				writeOpenAIError(w, http.StatusUnauthorized, "api_key_expired", "this API key has expired")
				return
			}
			if status != apikeys.StatusActive {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, info)))
			return
		}
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			// 常量时间比较（发现 7）：!= 短路时序随前缀长度变化，公网暴露下
			// 理论上可逐字节探测 key 前缀；ConstantTimeCompare 消除该信号。
			provided := strings.TrimPrefix(authz, "Bearer ")
			if !strings.HasPrefix(authz, "Bearer ") ||
				subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.APIKey)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

type internalAdminContextKey struct{}

// A managed calling key represents a client, not the account administrator.
// Explicit legacy single-key deployments retain their shared-key management API.
func (h *Handler) withAccountAdmin(next http.HandlerFunc) http.HandlerFunc {
	authenticated := h.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKeys != nil && r.Context().Value(internalAdminContextKey{}) != true {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "internal endpoint")
			return
		}
		authenticated(w, r)
	}
}

// apiKeyContextKey 携带本次请求使用的密钥信息（模型白名单等）。
type apiKeyContextKey struct{}

// routingSessionKeyContextKey preserves Responses routing hints across translation.
type routingSessionKeyContextKey struct{}

// requestKeyInfo 返回鉴权命中的密钥信息；单密钥模式或内部管理请求返回零值。
func requestKeyInfo(r *http.Request) (apikeys.Info, bool) {
	info, ok := r.Context().Value(apiKeyContextKey{}).(apikeys.Info)
	return info, ok
}

// modelAllowedByKey 判断请求模型是否在密钥白名单内。
//
// 匹配是「完整模型名逐字相等」，不做任何前缀解析或降级：
//   - 绑定 `cn:glm-5.2` 只放行请求里的 `cn:glm-5.2`，不放行 `global:glm-5.2`，
//     也不放行裸名 `glm-5.2`；
//   - 绑定裸名同样只放行裸名请求，不会因为 resolveModel 把裸名归到 cn 域
//     就顺带放行 `cn:glm-5.2`。
//
// 绑定值应当直接取自 `GET /v1/models` 的完整模型名，管理台的下拉框就是按这个
// 列表给的。此前的两种写法都属降级匹配：最初只比裸名、仅在绑定项自带 ":" 时
// 才校验 realm，裸名绑定会跨域放行；改成按 resolveModel 解析后再比较，又会把
// 裸名绑定静默扩成 `cn:` 域的两个名字。两者都会让实际可用范围大于管理员写下
// 的那一条，所以现在一律按字面比对，白名单里写什么就只放行什么。
//
// 白名单为空表示不限制，保持旧密钥行为。
func modelAllowedByKey(info apikeys.Info, requestModel string) bool {
	if len(info.Models) == 0 {
		return true
	}
	for _, allowed := range info.Models {
		if allowed == requestModel {
			return true
		}
	}
	return false
}

// InternalHandler 只挂载到权限为 0600 的 Unix socket，使面板管理不依赖任一调用密钥。
func (h *Handler) InternalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), internalAdminContextKey{}, true)))
	})
}

// requireInternal 限定只允许本机管理 socket（InternalHandler 注入的上下文）访问：
// 公开端口上同一个 mux 也会匹配这条路由，没有这道闸就等于把全量用量暴露给任一调用密钥。
func (h *Handler) requireInternal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(internalAdminContextKey{}) != true {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "internal endpoint")
			return
		}
		next(w, r)
	}
}

// usageStats 返回按密钥累计的 token 用量（管理台「用量统计」页数据源）。
func (h *Handler) usageStats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Usage == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "用量账本未启用：config 里设置 usage_file 后重启网关",
		})
		return
	}
	snapshot := h.cfg.Usage.Snapshot()
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"file":       h.cfg.Usage.Path(),
		"since":      snapshot.Since,
		"updated_at": snapshot.UpdatedAt,
		"totals":     snapshot.Totals,
		"keys":       snapshot.Keys,
		// days 是按天分桶（新到旧），面板的日期筛选读它；漏传会让筛选永远落在「全部」。
		"days": snapshot.Days,
	})
}

// recordUsage records one finished client request, independently of its final
// success status. Unknown fields are marked rather than estimated.
func (h *Handler) recordUsage(st *chatStat, model string) {
	if h.cfg.Usage == nil || st == nil {
		return
	}
	h.cfg.Usage.RecordOutcome(st.keyID, st.keyName, st.keyMask, model, st.prompt, st.toks, st.cached,
		st.credit, st.hasCred, usage.Outcome{Failed: st.failed, Unreported: st.unreported}, time.Now())
}

// updateStatus 返回热更新状态（当前版本、远端最新版本、最近错误）。
// reasoningLoopFeature 返回重复推理保护的运行期设置（供管理台渲染开关）。
func (h *Handler) reasoningLoopFeature(w http.ResponseWriter, r *http.Request) {
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"stop_only": h.reasoningLoopStopOnly(),
		"default":   h.cfg.ReasoningLoopStopOnly,
	})
}

// setReasoningLoopFeature 热切换「命中循环后是否只停不重发」。只接受显式布尔值：
// 这个开关决定失败会不会被一次重发吸收掉，静默接受空值或字符串会悄悄改变调用方
// 看到的行为。改完立即作用于后续请求，已经在跑的那次沿用开始时的语义。
func (h *Handler) setReasoningLoopFeature(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StopOnly *bool `json:"stop_only"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		_ = writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "请求体需为 {\"stop_only\": true|false}"})
		return
	}
	if body.StopOnly == nil {
		_ = writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "stop_only 必须是 true 或 false"})
		return
	}
	h.SetReasoningLoopStopOnly(*body.StopOnly)
	log.Printf("INFO: [server] reasoning loop stop-only set to %t via admin channel", *body.StopOnly)
	_ = writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stop_only": *body.StopOnly})
}

func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "enabled": false,
			"message": "热更新未启用：config 里设置 update.enabled=true 后重启网关",
		})
		return
	}
	_ = writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": h.cfg.Update.Status()})
}

// reviveAccount 人工复活被停用的账号（仅管理通道）：清除 disabled 与原因，账号回到
// 池中（若无其他冷却/熔断则立即可选）。
//
// 需要它的原因：上游会把「内容未通过安全审核」也回成 11140，早前版本据此硬禁用账号
// （2026-09-27 两个健康号被误停用）；修正分类后仍可能有其它误判，运维需要一个恢复入口。
func (h *Handler) reviveAccount(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "账号池不可用"})
		return
	}
	var body struct {
		UID string `json:"uid"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "读取请求体失败"})
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "请求体必须是 JSON 对象"})
			return
		}
	}
	uid := strings.TrimSpace(body.UID)
	if uid == "" {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "需要 uid"})
		return
	}
	before, known := h.cfg.Pool.Status(uid)
	if !known {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "账号不存在"})
		return
	}
	h.cfg.Pool.ReviveDisabled(uid)
	after, _ := h.cfg.Pool.Status(uid)
	log.Printf("INFO: [server] account revived uid=%s (was disabled=%v reason=%q)",
		logfmt.UID8(uid), before.Disabled, before.DisabledReason)
	_ = writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "disabled": after.Disabled, "was_disabled": before.Disabled})
}

// updateCheck 查询远端最新版本（只读，不改动任何东西）。
func (h *Handler) updateCheck(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "热更新未启用"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, err := h.cfg.Update.Check(ctx)
	if err != nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "status": status})
		return
	}
	_ = writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

// updateApply 触发一次热更新。
//
// 下载与交接在后台完成（以秒计），接口立即返回；真正的停机发生在交接成功之后，
// 由 main 的优雅停机路径把在途请求跑完。前端用 /update 轮询进度。
func (h *Handler) updateApply(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Update == nil {
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "热更新未启用"})
		return
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if r.Body != nil {
		// 载荷可以完全省略（管理台发 {}），但一旦带了内容就必须是合法的小 JSON，
		// 否则一次手滑的请求会静默变成"升到最新版"。
		raw, readErr := io.ReadAll(io.LimitReader(r.Body, (1<<12)+1))
		if readErr != nil {
			_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "读取请求体失败"})
			return
		}
		if len(raw) > 1<<12 {
			_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "请求体过大"})
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
				return
			}
		}
	}
	status := h.cfg.Update.Status()
	if !status.Enabled {
		// 关掉热更新后必须如实拒绝：此前这里不检查开关，接口会回「已开始热更新」，
		// 而实际什么都没发生（只留一条 ERROR 日志），面板显示"正在更新"却永远不动。
		_ = writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": "自更新未启用（config update.enabled=false）；升级请手工部署", "status": status,
		})
		return
	}
	switch status.State {
	case hotupdate.StateChecking, hotupdate.StateDownloading, hotupdate.StateHandover:
		_ = writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "已有更新任务在进行中", "status": status})
		return
	}
	target := strings.TrimSpace(body.Tag)
	// 只有「已经查过远端、且确认没有新版本」才直接拒绝。没查过就交给 Apply 自己去查，
	// 否则管理台必须先点一次「检查更新」才能升级，用户看到的是莫名其妙的"已经是最新"。
	if !status.UpdateReady && target == "" && !status.CheckedAt.IsZero() {
		_ = writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "message": fmt.Sprintf("已经是最新版本（当前 %s，远端 %s）", status.Current, status.LatestTag),
			"status": status,
		})
		return
	}
	go func() {
		// 独立 ctx：请求返回后这次下载仍要跑完。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := h.cfg.Update.Apply(ctx, target); err != nil {
			log.Printf("ERROR: [update] apply failed: %v", err)
		}
	}()
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": "已开始热更新：新实例接管后，本实例会把手上的请求跑完再退出",
		"status": h.cfg.Update.Status(),
	})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	_ = writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"version":        version.Version,
		"commit":         version.Commit,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
	// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底）。
// 绑定模型的密钥只会看到自己可用的模型，客户端据此选择也不会撞 403。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.visibleModels(r),
	})
}

// modelsRefresh 强制绕过模型列表缓存重新拉取一次（管理台「刷新模型」按钮）。
//
// 模型列表（含 credits 倍率）缓存 1 小时，是刻意的：/v1/models 是展示端点，
// 每次都打上游会把展示变成高频调用，失败时还会反复重试。代价是上游调整倍率后
// 面板最多滞后 1 小时。这个端点给运维一个按需刷新入口——不缩短常态 TTL，
// 只在有人主动点的时候真拉一次。
//
// 只作废缓存并重新拉取，不改动任何模型/密钥绑定状态。拉取失败时如实回错，
// 不把「没刷新成功」伪装成成功（面板据此提示，而不是显示旧数据说已刷新）。
func (h *Handler) modelsRefresh(w http.ResponseWriter, _ *http.Request) {
	before := h.fetchDynamicModels()
	h.invalidateModelCaches()
	after := h.fetchDynamicModels()
	if len(after) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "models_unavailable",
			"could not refresh the model list from the upstream; the previous snapshot is unchanged")
		return
	}
	// 两域都刷：global 名单有独立缓存，只刷 CN 会让面板上的 global 倍率仍滞后。
	globalNames, _ := h.fetchGlobalModels()
	_ = writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"before":    len(before),
		"after":     len(after),
		"global":    len(globalNames),
		"refreshed": true,
	})
}

// invalidateModelCaches 作废两域模型缓存，让下一次读取真的回源。
// 置零 fetched 即失效（读取侧判的是 time.Since(fetched) < TTL）；同时清掉
// lastFail，否则刚失败过会让刷新请求撞上 5 分钟负缓存而看不到新数据。
func (h *Handler) invalidateModelCaches() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	h.cfg.Upstream.InvalidateGlobalModels()
}

// globalModels 国际版（global realm）模型名名单（PLAN §7.2 附录 21 名）——已删。
// 纯动态化后 handler 不再持有任何静态名单：无 global 账号 / 探测失败 → 空列表。

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
// normalizeCredits 把上游 credits 原文规整成纯倍率写法（"x0.34"）。
//
// 上游同一字段有两种形态：`x0.34` 与 `x0.34 credits`（2026-10-10 线上实测：
// 73 个模型里 4 个带后缀，全是 global 侧）。原样透传会让 `credits` 出现两种形状，
// 下游按它做解析或比较时很容易踩坑——这里统一，同时保留「上游没给」与
// 「给了空值」的区别（都返回 ""，调用方按空值省略字段，不编造）。
func normalizeCredits(raw string) string {
	s := strings.TrimSpace(raw)
	// 去掉 "credits" 后缀（大小写不敏感），再清一次空白。
	if len(s) >= len("credits") && strings.EqualFold(s[len(s)-len("credits"):], "credits") {
		s = strings.TrimSpace(s[:len(s)-len("credits")])
	}
	return s
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
func fmtCreditsPrefix(raw string) string {
	s := normalizeCredits(raw)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		// 规整成纯倍率写法（去掉上游偶发的 " credits" 后缀），仅展示用。
		if c := normalizeCredits(mi.Credits); c != "" {
			entry["credits"] = c
		}
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底；
// global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		if mi.ContextWindow > 0 {
			entry["context_length"] = mi.ContextWindow
		}
		if mi.MaxTokens > 0 {
			entry["max_output_tokens"] = mi.MaxTokens
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// P0：effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （issue #84 客户端可发现档位，不再盲传）。无档位→省略字段（非空数组）。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 探测结果（fetchGlobalModels 纯动态，失败/无号 → 空）；无 global 账号时
	// 空名单且零上游调用。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				// 两域均只公布已知的真实限制；上游没给时保持未知，避免客户端
				// 把兼容兜底值当成模型窗口并触发错误的压缩策略。
				if mi.ContextWindow > 0 {
					entry["context_length"] = mi.ContextWindow
				}
				if mi.MaxTokens > 0 {
					entry["max_output_tokens"] = mi.MaxTokens
				}
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无 global 账号 → 空名单 + nil 账号（不发起上游调用）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := jsonutil.Decode(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。
// 只从 CN realm 账号拉取（PickExcludingForRealm(nil,"","cn")）：全局账号的模型列表
// 未必与 CN 一致，动态模型表只服务 CN 前缀（global 走独立探测）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "cn")
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// rejectUnsupportedChoiceCount 拒绝 n>1：上游只返回一个选择，静默降级会让客户端
// 按 n 取第二个选择时报错（与 previous_response_id 一样按「明确拒绝」处理）。
func rejectUnsupportedChoiceCount(body []byte) error {
	var request struct {
		N *json.Number `json:"n"`
	}
	if json.Unmarshal(body, &request) != nil || request.N == nil {
		return nil
	}
	count, err := request.N.Int64()
	if err != nil {
		return nil
	}
	if count < 1 {
		return fmt.Errorf("n must be at least 1")
	}
	if count > 1 {
		return fmt.Errorf("n=%s is not supported: this upstream returns a single choice", request.N.String())
	}
	return nil
}

// stripContextMarker 去掉 Anthropic 的上下文窗口别名后缀（`[1m]`／`[1M]`）。
// 返回清理后的名字与是否发生了改动；去掉后为空则原样返回（不改动畸形输入）。
func stripContextMarker(name string) (string, bool) {
	trimmed := strings.TrimSpace(name)
	if len(trimmed) > 4 && strings.EqualFold(trimmed[len(trimmed)-4:], "[1m]") {
		if base := strings.TrimSpace(trimmed[:len(trimmed)-4]); base != "" {
			return base, true
		}
	}
	return name, false
}

// maxModelNameBytes 请求模型名长度上限（含 realm 前缀）。
const maxModelNameBytes = 256

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（超过上限一半）：小探针（{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body)*2 >= int(limit) {
		if err := os.WriteFile("/app/data/last_request.json", body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	var requestObject map[string]json.RawMessage
	if err := json.Unmarshal(body, &requestObject); err != nil || requestObject == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid request fields: "+err.Error())
		return
	}
	// n>1 需要上游返回多个选择，当前上游不提供：静默只回一个 choice 会让按 n 取值
	// 的客户端越界，明确拒绝。
	if err := rejectUnsupportedChoiceCount(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Anthropic 的 1M 上下文别名（`model[1m]`／`[1M]`）：Claude Code 会把带后缀的模型名
	// 原样发出来（cc-switch 的「声明支持 1M」开关就会产生它），而本网关的模型目录与
	// 密钥白名单只有裸名。按同一模型处理：去掉后缀，并把请求体里的模型名一并归一。
	if clean, stripped := stripContextMarker(peek.Model); stripped {
		log.Printf("INFO: [server] model context marker stripped: %q -> %q", peek.Model, clean)
		peek.Model = clean
		body = rewriteModel(body, clean)
	}
	if err := validateChatRequest(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 模型名进入账本键、请求明细和日志行：不设上限时，持有密钥的调用方可用超长名字
	// 撑爆用量文件的写盘上限，让所有用量停止落盘。真实模型名远短于此。
	if len(peek.Model) > maxModelNameBytes {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("model name exceeds %d bytes", maxModelNameBytes))
		return
	}
	if r.URL.Path == "/v1/chat/completions" && peek.Stream && hideChatStreamUsage(requestObject) {
		w = &chatUsageVisibilityWriter{inner: w}
	}
	if _, responses := w.(*responsesWriter); !responses {
		contract, err := newChatOutputContract(requestObject)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		warnIgnoredBuiltinToolsJSON(w, r, requestObject["tools"])
		changed, err := normalizeChatToolDeclarations(requestObject, contract)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if changed {
			body, err = json.Marshal(requestObject)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
				return
			}
		}
		if contract != nil {
			// lastWrite 刻意保持零值：首帧被缓冲（工具调用要整组校验）时，零值会让
			// 契约写入器立刻发一条 ": keepalive" 注释——它同时把响应头先送出去，
			// 客户端才不会在整组工具参数到齐前一直等不到响应。见 review 测试
			// TestMessagesReviewClientCancellationStopsUpstream。
			checked := &chatContractWriter{inner: w, req: contract}
			w = checked
			defer checked.finish()
		}
	}

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)

	// global → 同名 CN 模型回落（按密钥开关，见 apikeys.Info.GlobalFallbackToCN）：
	// 仅当该密钥开启、请求带 global: 前缀、且 global 域在**这个模型**上确实一个可服务的
	// 号都没有（不可用的号全部因限流或禁用）时，才把域切到 cn 用同名裸模型选号。
	// 存在熔断、在途占满等其它不可用原因时不回落——那些是暂时状态或与限流无关，
	// 静默改道到另一个域会让调用方看到与预期不符的模型行为。
	// 回落只改选号域与出站路由：模型名（裸名）与请求体都不变，账本仍记在调用方请求的名字上。
	if realm == "global" && h.cfg.Pool != nil {
		if info, ok := requestKeyInfo(r); ok && info.GlobalFallbackToCN != nil && *info.GlobalFallbackToCN {
			if state := h.cfg.Pool.RealmRateStateForModel("global", bareModel); state.AllRateLimited() {
				if cnState := h.cfg.Pool.RealmRateStateForModel("cn", bareModel); cnState.Available > 0 {
					log.Printf("INFO: [server] global realm rate limited for model=%s — falling back to cn by key request", bareModel)
					realm = "cn"
				}
			}
		}
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	st.requestID = requestID(r)
	st.trace = traceFor(r)
	if st.trace != nil {
		st.trace.stat = st
		st.trace.record.Model = traceText(peek.Model)
		st.trace.record.Stream = peek.Stream
		st.start = st.trace.record.StartedAt
	}
	defer st.done()
	defer func() {
		// Protocol adapters may buffer their final JSON/SSE frame. Walk the
		// complete chain before accounting, including adapters below a tool
		// contract or visibility wrapper.
		if err := finishResponseWriters(w); err != nil {
			st.failed = true
			if st.status < 400 {
				st.status = http.StatusBadGateway
			}
			st.trace.finishResponseError(w, err, r.Context())
		}
		if r.Context().Err() != nil {
			st.failed = true
			st.status = 499
		}
		if st.upstreamStarted {
			h.recordUsage(st, peek.Model)
		}
	}()
	// 调用方密钥身份：请求行 key= 列与用量账本都按它归属（单密钥模式没有 Info）。
	if info, ok := requestKeyInfo(r); ok {
		st.keyID, st.keyName, st.keyMask = info.ID, info.Name, info.MaskedKey
	}

	// 密钥模型绑定：只放行白名单内的模型，拒绝发生在选号之前——不占用账号、不轮转、不冷却。
	if info, ok := requestKeyInfo(r); ok && !modelAllowedByKey(info, peek.Model) {
		writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
			fmt.Sprintf("this API key is restricted to its bound models and cannot use %q", peek.Model))
		st.status = http.StatusForbidden
		return
	}

	reasoningLoopGuard := h.cfg.ReasoningLoopGuard == nil || *h.cfg.ReasoningLoopGuard
	if info, ok := requestKeyInfo(r); ok && info.ReasoningLoopGuard != nil {
		reasoningLoopGuard = *info.ReasoningLoopGuard
	}
	tried := map[string]bool{}
	var lastErr error
	// loopRetries 统计本次请求内「重复推理循环 + 客户端零字节」的重发次数（同账号重发）。
	// 上限见 maxReasoningLoopRetries：循环是上游模型行为，重发通常能拿到干净的一轮，
	// 但持续循环时必须收手并如实回报错误，不能无限重试。
	loopRetries := 0
	// softRotate 统计「语义上可换号再试」的终态错误（内容审核、未知 4xx）已重试的次数：
	// 这些错误此前直接回给调用方，但内容审核可能带账号/风控维度、未知 4xx 也可能是该账号
	// 的权限问题，换号成本只是延迟（被拒请求不产生上游计费）。试满 maxSoftRotations 次
	// 仍失败才把上游原文交给调用方。
	softRotate := 0
	// reviewRejectedUIDs 记录本次客户端请求里「被内容审核拒绝过」的账号。
	// 一旦同一次请求里另有账号成功返回，就说明同一批正文在别的号上能过——那些被拒
	// 的号是账号维度被上游标记，立即停用（见 pool.FlagReviewAccount）。这比「连续 N 次」
	// 的弱证据准确得多，且能当场止损，不用等它再被选中两次。
	var reviewRejectedUIDs []string
	// readRotate 统计「上游掐流且客户端还没收到任何内容」时已换号重试的次数。
	// 上游偶发 INTERNAL_ERROR / 连接中断（2026-09-30 实测约占请求的 0.7%），此时换号
	// 重发往往能成功；但只在**客户端还什么都没收到**时才安全——已经推过内容再重发
	// 会让客户端看到重复或矛盾的两段输出。上限见 maxReadRotations。
	readRotate := 0

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey, preserved := r.Context().Value(routingSessionKeyContextKey{}).(string)
	if !preserved {
		sessKey = session.ExtractKey(body)
	}
	sessKey = session.ScopeKey(st.keyID, sessKey)

	// stickyKey 只用于「绑定/解绑账号」，与上方聚合用的 sessKey 分开：
	// 部分 OpenAI 兼容客户端（实测 narrafork 的 /v1/chat/completions）请求体顶层
	// 只有 model / messages / stream / tools 等，既无 conversation_id 也无
	// metadata、prompt_cache_key，ExtractKey 恒返回空串 → 会话粘性对它们完全失效，
	// 同一对话每轮换号、上游提示缓存整段失效（线上表现为这类请求 hit 恒为 0）。
	// 这里在无显式会话标识时，从正文派生对话级回退键补上粘性；显式标识仍优先。
	//
	// 不并进 sessKey：sessKey 还决定会话头族的聚合粒度（无显式标识时按「轮」聚合，
	// 见下方 turnKey），把回退键混进去会把聚合粒度从轮级改成对话级，属于另一件事。
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = session.ScopeKey(st.keyID, session.ContentKey(body))
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级兜底聚合键：无会话键的客户端（OpenAI 兼容协议——dsh / Codex / Cherry
	// Studio 等请求体里既无 conversationId 也无 metadata）sessKey 恒空，会话头族的
	// 聚合主键此前只能逐请求新生成，agent 多轮在上游用量明细里仍是一条请求一条记录。
	// 这里按 body 里最后一条 user 消息派生轮级键（同轮内所有上游调用同键）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := ""
	if sessKey == "" {
		turnKey = session.ScopeKey(st.keyID, session.TurnKey(body))
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	writeContextFailure := func(detail string) {
		releaseHeld()
		st.status = http.StatusBadRequest
		// 流已经开始时必须在流内交付失败：往已开始的 SSE 流里写 JSON 错误体会让
		// 客户端解析失败（Responses 走 response.failed，Messages/Chat 走 error 事件）。
		if deliverStreamFailure(w, "context_length_exceeded", detail) {
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "context_length_exceeded", detail)
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.UnbindIfUID(stickyKey, stickyUID)
			stickyUID = ""
		}
	}

	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 只有显式 custom 配置才替换 system/developer；passthrough 始终保留原文。
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	}

	// 运行约定：带工具的请求注入（见 prompt.ActNote）。
	//
	// 此前只在 /v1/responses 里调用 applyActNote，走 /v1/chat/completions 的客户端
	// （Devin、narrafork 等）拿不到这条约定：模型回一句「让我先确认…」就结束本轮，
	// 客户端不再自动继续，用户只能手动发「继续」。
	//
	// **必须在 prompt.Rewrite 之后**：Rewrite 会删掉所有 system/developer 消息再
	// 插入配置提示词，而约定正是追加到 system 消息里。放在它前面时，custom 模式下
	// 约定会被连同旧 system 一起删掉，永远到不了模型——文档却承诺它一定生效
	// （2026-09-30 深度体检发现）。
	//
	// 工具声明看出站 chat 体（Responses 路径转换后同样带 tools 字段）。
	body = applyActNote(body, h.cfg.PromptActNote, chatBodyHasTools(body))

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取，缺省空串；多密钥模式先按调用方隔离，
	//     客户端原始正文保持不变，单密钥模式保持原值；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 优先（同样按调用方
	//     隔离），否则按粘性 key 进程内稳定生成（同会话
	//     恒同值）；粘性 key 也为空时走轮级兜底（session.TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级 NewMessageID——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ScopeKey(st.keyID, session.ResolveConversationID(body))}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = session.ScopeKey(st.keyID, v)
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键客户端：轮级兜底——同轮内 tool call 多轮 / 换号重试 / 降级重发
		// 共享同键，用户发下一条消息自动换键。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = session.ScopeKey(st.keyID, r.Header.Get("X-Trace-ID"))
	chatMeta.GatewayRequestID = st.requestID
	chatContext := upstream.WithChatRetryObserver(r.Context(), st.absorbJSONUsage)
	chatContext = upstream.WithChatAttemptObserver(chatContext, func(event upstream.ChatAttemptEvent) {
		st.observeAttempt(event)
		if event.Stage == "start" {
			// 让流式适配器知道上游请求已经发出：此后长时间没有首帧就可以先开流并 ping，
			// 避免客户端在循环保护压制期（最长 60 秒）一个字节都收不到。
			for current := w; current != nil; {
				if starter, ok := current.(interface{ UpstreamStarted() }); ok {
					starter.UpstreamStarted()
					break
				}
				next, ok := current.(interface{ Unwrap() http.ResponseWriter })
				if !ok {
					break
				}
				current = next.Unwrap()
			}
		}
	})
	// 输出预算按会话推算：会话键交给出站层；成功后记录本轮上游实报输入，供下一轮主动收缩预算。
	chatContext = upstream.WithBudgetSession(chatContext, stickyKey)
	defer func() {
		if !st.failed && st.status > 0 && st.status < 400 && st.lastPrompt > 0 && r.Context().Err() == nil {
			h.cfg.Upstream.RecordPromptSample(stickyKey, realm, bareModel, int64(st.lastPrompt), body)
		}
	}()

	// 本请求等待「在途名额释放」的总截止时间：整个轮转循环共享一份预算。
	// 若每轮换一次号都重新计一份，客户端等待会被放大到 MaxRotate 倍；
	// 而 slotWait<=0（显式关闭）时该时刻已过期，下面的判定恒为假 → 立即 503。
	slotWaitDeadline := time.Now().Add(h.slotWait)

	for i := 0; i < h.cfg.MaxRotate; i++ {
		if r.Context().Err() != nil {
			st.status = 499
			return
		}
		// 来源级限流闸门（见 pool/sourcerate.go）：本轮已经确认上游在按来源限流时，
		// 继续换号只是把更多账号送去撞墙（实测一次客户端请求烧掉 3 个号，最后以
		// `exceeded retry limit, last status: 429` 收场）。这里提前收手并如实回 429。
		// 只在**轮转循环内**判定：新请求的首个尝试不拦（上游限流通常很短，一刀切拒绝
		// 新请求反而伤可用性），由第一次尝试自己去探测上游是否已恢复。
		if i > 0 {
			if limited, remaining := h.cfg.Pool.SourceRateGate(realm); limited {
				st.status = http.StatusTooManyRequests
				writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
					fmt.Sprintf("upstream is rate limiting this gateway's source; retry in about %ds",
						int(remaining.Seconds())+1))
				return
			}
		}
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			var decision pool.Decision
			acct, decision = h.cfg.Pool.PickByUIDForModelRealmWithDecision(stickyUID, bareModel, realm)
			st.trace.decision(decision)
			if acct == nil {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			}
		}
		// decision 保存最近一次失败选号的事实，供下面的「等待名额」判定使用。
		var decision pool.Decision
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct, decision = h.cfg.Pool.PickExcludingForRealmWithDecision(tried, bareModel, realm)
			st.trace.decision(decision)
		}
		// 健康账号只是全被在途名额占满时，有界等待一个名额释放后重选。
		// 这是秒级瞬时状态（既有请求一结束名额就释放），直接回 503 会把并发
		// 峰值误报成「无号可用」；而「无健康账号」（全禁用/冷却）不在此列——
		// 那种情况几秒内不会改变，等待只会白让客户端等（见 slotWaitable）。
		if acct == nil && slotWaitDeadline.After(time.Now()) && slotWaitable(decision) {
			acct, decision = h.waitForSlot(r.Context(), tried, bareModel, realm, slotWaitDeadline)
			st.trace.decision(decision)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.trace.selected(acct, bareModel)
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
					// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		//
		// 注册地补交（global 14017）：账号若没在官方登录流程里确认国家/地区，chat 会被
		// 上游以 429 + code 14017（trial not activated）永久拒绝——它不会自愈，只会被
		// 反复冷却轮换掉，池子越用越小。命中该形态时补交一次注册地（幂等），然后用
		// **同一个账号**重发一次：实测补交后同号立即从 429 变 200。只重发一次，
		// 且只对本请求内已占用的这个账号做，不额外占用其他号。
		var rc io.ReadCloser
		var status int
		var respBody []byte
		var terr error
		// 同一账号内的重发轮次：14017 注册地补交（一次）与重复推理循环（有界）都走这里。
		// 循环重发必须是**同一个账号**：换号会丢掉刚建立的上下文缓存，而循环是模型行为、
		// 与账号无关，换号解决不了问题。
		regionRepaired := false
		for {
			rc, status, respBody, terr = h.cfg.Upstream.ChatStreamContext(chatContext, acct, body, clientIP, chatMeta)
			if terr != nil || status < 400 || regionRepaired {
				break
			}
			if upstream.Classify(status, string(respBody)) != upstream.ErrAccountFault {
				break
			}
			if !upstream.RegionRequired(status, string(respBody)) {
				break
			}
			// 命中即先留观测：无论补交成败，面板都该看到「这个号最近被上游拒过」。
			// 用 NoteTransientError 而非 NoteError——14017 是配置缺失不是账号故障，
			// 补交成功即恢复，不该污染成功率权重。
			h.cfg.Pool.NoteTransientError(acct.UID)
			repair := h.cfg.RegionRepair
			if repair == nil {
				repair = h.cfg.Upstream.CompleteRegion
			}
			area, rerr := repair(acct)
			if rerr != nil {
				log.Printf("WARN: [server] region repair failed uid=%s: %v", logfmt.UID8(acct.UID), rerr)
				break
			}
			log.Printf("INFO: [server] region repair uid=%s country=%s (%s) — retrying same account",
				logfmt.UID8(acct.UID), area.IOS2, area.EnName)
			st.absorbJSONUsage(respBody)
			regionRepaired = true
		}
		if terr != nil {
			st.attemptError = traceTransportCode(r.Context())
			st.absorbUsage(nil)
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.absorbJSONUsage(respBody)
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// 以下几类终态由请求本身决定、与账号无关：只释放在途名额，保留会话绑定，
			// 下一轮仍落在同一账号，上游提示缓存不失效（换号会让整段前缀重新计费）。
			if kind == upstream.ErrChannelRejected {
				releaseHeld()
				writeOpenAIError(w, http.StatusBadRequest, "upstream_channel_rejected",
					"upstream rejected the client channel: Illegal API invocation from an unapproved channel")
				st.status = http.StatusBadRequest
				return
			}
			// 上游 WAF 按请求正文特征拦截（XSS/SQL 规则），返回的是 HTML 拦截页而非 JSON。
			// 判定看正文、不看账号：同域内换任何号都会撞同一条规则，因此请求终态——
			// 不轮转、不罚账号，也不把 HTML 页当作「请求参数被拒」回显给调用方。
			if kind == upstream.ErrUpstreamWAF {
				releaseHeld()
				writeOpenAIError(w, http.StatusBadRequest, "upstream_waf_blocked",
					"upstream WAF rejected this request even after the gateway broke the matching "+
						"patterns; start a new conversation, or remove the HTML/script/SQL-looking part")
				st.status = http.StatusBadRequest
				return
			}
			// 上游代理层 HTML 授权页（APISIX / openresty 等）：2026-09-22 实测的形态是
			// 风控收紧期间整域返回 401 HTML，几分钟后自行恢复。
			//
			// 三件事都不能做：不把 HTML 回显给调用方（此前它落进 bad params 分支，
			// 客户端只能看到一坨 `<html>`）；不罚账号（同一时段该账号刷新令牌后仍被拒，
			// 说明拒绝发生在代理层而不是账号上）；不当作请求终态直接失败——它是**可重试**
			// 的临时状态，回 503 让客户端稍后重试，与「无健康账号」同口径。
			//
			// 这里刻意不调 fail(acct.UID)：fail 会顺带解绑会话粘性，而拒绝是整域级别的、
			// 与这个账号无关，解绑只会让下一轮无谓地重新选号。在途租约由函数出口的
			// defer 释放，不依赖 fail。
			if kind == upstream.ErrUpstreamGateway {
				log.Printf("WARN: [server] upstream gateway page uid=%s status=%d — upstream proxy rejected the request; account untouched",
					logfmt.UID8(acct.UID), status)
				st.status = http.StatusServiceUnavailable
				writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_gateway_unavailable",
					"upstream proxy temporarily refused the request (not an account or request problem); retry in a moment")
				return
			}
			// 上游内容拒绝属于当前请求；直接返回，不修改其他会话或替换正文重试。
			if kind == upstream.ErrContentBlocked {
				// 本次拒绝先按连续计数记账（达到阈值即停用）；同时记下这个号，
				// 一旦本轮请求另有账号成功，就把它升级为「账号被标记」并立即停用。
				if h.cfg.Pool.NoteContentBlocked(acct.UID) {
					log.Printf("WARN: [server] content review rejection — disabling account "+
						"uid=%s reason=flagged-by-review (hit the %d-rejection threshold with no success in between) model=%s",
						logfmt.UID8(acct.UID), pool.ReviewFailThreshold(), bareModel)
				}
				reviewRejectedUIDs = append(reviewRejectedUIDs, acct.UID)
				// 内容审核：先换号再试（审核可能带账号/风控维度；换号成本只是延迟，
				// 被拒请求不计费），试满才把上游原文交给调用方。
				if softRotate < h.cfg.MaxSoftRotations {
					softRotate++
					log.Printf("INFO: [server] content review rejection — trying another account (%d/%d) uid=%s model=%s",
						softRotate, h.cfg.MaxSoftRotations, logfmt.UID8(acct.UID), bareModel)
					// fail 而不是 releaseHeld：必须解绑会话粘性，否则下一轮又会选中同一个
					// 账号（实测：被风控标记的号在粘性会话里被反复选中，换号形同虚设）。
					fail(acct.UID)
					continue
				}
				// 不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样，
				// 任务书授权上游错误码/账号语义对客户端可见），不再改写成网关固定文案。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				releaseHeld()
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIError(w, http.StatusBadRequest, "content_blocked", msg)
				st.status = http.StatusBadRequest
				return
			}
			if kind == upstream.ErrContextTooLong {
				// 上下文超限：请求体本身超模型上限，换任何账号都是同一结果。
				// 立即回客户端并透传上游原文（含真实 token 数与上限），不轮转、不罚账号。
				// 让调用方看到 "prompt is too long: N tokens > M maximum" 自行压缩或开新会话。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
				// 不再额外加 "prompt is too long: " 前缀——上游 msg 本身就以它开头，
				// 硬加会得到 "prompt is too long: prompt is too long: N tokens > M maximum"。
				writeContextFailure(upstream.ContextTooLongDetail(string(respBody)))
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			if kind == upstream.ErrBadParams || kind == upstream.ErrClient {
				// 未知 4xx 也可能是该账号的权限/状态问题：同样先换号再试；
				// 请求体本身有问题（ErrBadParams）换号无意义，维持原行为。
				if kind == upstream.ErrClient && h.cfg.RotateOnClientError && softRotate < h.cfg.MaxSoftRotations {
					softRotate++
					log.Printf("INFO: [server] upstream client error — trying another account (%d/%d) uid=%s model=%s status=%d",
						softRotate, h.cfg.MaxSoftRotations, logfmt.UID8(acct.UID), bareModel, status)
					fail(acct.UID) // 同上：解绑粘性才能真正换号
					continue
				}
				releaseHeld()
				detail := string(respBody)
				acct.Lock()
				secretValues := []string{acct.AccessToken, acct.RefreshToken, acct.DeviceToken}
				acct.Unlock()
				secretValues = append(secretValues, h.cfg.APIKey, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
				for _, secret := range secretValues {
					if len(secret) > 4 {
						detail = strings.ReplaceAll(detail, secret, "[redacted]")
					}
				}
				if len(detail) > 2000 {
					detail = logfmt.Truncate(detail, 2000)
				}
				// 11155（reasoning_content_missing）归因：上游只看「assistant 消息有没有
				// reasoning_content 字段」，出错时把出站消息形状压成一行，复现即可定位
				// 是客户端没带推理项、转换层丢了文本，还是字段缺失。不含任何正文。
				if strings.Contains(detail, "reasoning_content_missing") || strings.Contains(detail, `"code":11155`) {
					stats, hasStats := reasoningStatsFrom(r.Context())
					log.Printf("WARN: [server] upstream 11155 shape uid=%s key=%s %s",
						logfmt.UID8(acct.UID), st.keyName, chatShapeSummary(body, stats, hasStats))
				}
				writeOpenAIError(w, http.StatusBadRequest, "upstream_invalid_request", "upstream rejected request params: "+detail)
				st.status = http.StatusBadRequest
				return
			}
			// 来源级限流判定（见 pool/sourcerate.go）：无重置时间的 429 实测会在
			// 2 分钟内打中十余个不同账号，是出口/来源限流而非账号问题。先记账，
			// 达到阈值即开闸门，后续请求在选号前就被拦住，不再逐号送死。
			// 带重置时间的 6004 不参与：那是模型级限额，按既有模型豁免切号即可。
			if kind == upstream.ErrSoftRate {
				if _, hasReset := upstream.ParseRateReset(string(respBody)); !hasReset {
					if h.cfg.Pool.NoteSourceRateLimit(realm, acct.UID) {
						log.Printf("WARN: [server] source-level rate limit detected uid=%s realm=%s — pausing selection for this realm",
							logfmt.UID8(acct.UID), realm)
					}
				}
			}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel)
			fail(acct.UID)
			continue
		}
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		stats := newChatStatsReaderSince(rc, st.start)
		streamOptions := upstream.StreamOptions{
			Model:              peek.Model,
			ReasoningLoopGuard: reasoningLoopGuard,
		}
		// 本次请求一开始就快照「命中后是否只停不重发」：管理台在请求进行中切换开关时，
		// 已经在跑的这一轮沿用开始时的语义，不会出现重发到一半忽然改判。
		stopOnly := h.reasoningLoopStopOnly()
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			var streamErr error
			var loopErr *upstream.StreamError
			// 同一账号内的循环重发循环。每轮先告诉 Stream「还剩几次重发机会」：
			//   - 还有机会 → 命中循环且客户端零字节时 Stream 不写任何字节，返回 Retryable；
			//   - 机会用尽 → Stream 按原有方式把错误如实写给客户端，不会出现"既不重发也不报错"。
			// 重发上限 maxReasoningLoopRetries，避免持续循环时无限重试。
			// 压制期回调：循环保护压住输出时先把流开起来/发心跳（同一 goroutine）。
			streamOptions.OnHold = func() { holdProgress(w) }
			for {
				// 单项停止开关打开时不给重发额度：Stream 会把错误如实写给客户端，
				// handler 这里也就不会进入下面的重发分支。
				streamOptions.LoopRetryAvailable = !stopOnly &&
					loopRetries < maxReasoningLoopRetries
				streamErr = upstream.Stream(w, stats, streamOptions)
				if streamErr == nil || !upstream.IsLoopGuardError(streamErr) ||
					!errors.As(streamErr, &loopErr) || !loopErr.Retryable ||
					loopRetries >= maxReasoningLoopRetries || r.Context().Err() != nil {
					break
				}
				// 被丢弃的这一轮上游确实生成并计费了（保护是在读到循环后才截断的），
				// 用量照实累计，不能因为重发就把它抹掉。
				st.attemptError = traceStreamCode(streamErr, r.Context())
				st.absorbUsage(stats)
				// 上一轮的流已经读完或已放弃，关闭失败不改变本轮结论。
				_ = rc.Close()
				loopRetries++
				log.Printf("INFO: [server] %s uid=%s model=%s — retrying same account (attempt %d/%d)",
					loopErr.Message, logfmt.UID8(acct.UID), bareModel, loopRetries, maxReasoningLoopRetries)
				// 重发前重建读取器：上一轮已知用量已经入账，不能随本轮丢失或重复累加。
				// 重发若被上下文预检拒绝，返回这次真实错误，不能用上一轮循环错误覆盖它。
				rc, status, respBody, terr = h.cfg.Upstream.ChatStreamContext(chatContext, acct, body, clientIP, chatMeta)
				if detail, contextFailure := upstream.ContextTooLongHTTPDetail(status, respBody); terr == nil && contextFailure {
					st.absorbJSONUsage(respBody)
					st.unreported = true
					writeContextFailure(detail)
					return
				}
				if terr != nil || status >= 400 {
					// 重发没能建立（传输层失败或上游直接报错）：Stream 已经压制了上一次
					// 的错误帧，这里必须把失败如实交给客户端，否则会静默结束。
					retryErr := st.absorbRetryFailure(r.Context(), status, respBody, terr)
					observeResponseError(w, retryErr.Code)
					// writeOpenAIError 自己会先问适配器能不能把失败交付在流内，
					// 因此这里只需按「已开流优先适配器、否则原始 SSE 错误帧」的顺序调用。
					if !deliverStreamFailure(w, retryErr.Code, retryErr.Message) {
						upstream.WriteStreamError(w, retryErr)
					}
					st.unreported = true
					st.status = http.StatusBadGateway
					if r.Context().Err() != nil {
						st.status = 499
					}
					return
				}
				stats = newChatStatsReaderSince(rc, st.start)
			}
			if checker, ok := w.(interface{ CompletionError() error }); ok && streamErr == nil {
				streamErr = checker.CompletionError()
			}
			if streamErr != nil {
				st.attemptError = st.streamAttemptCode(streamErr, stats, w, r.Context())
			}
			st.absorbUsage(stats)
			if streamErr != nil {
				// 上一轮的流已经读完或已放弃，关闭失败不改变本轮结论。
				_ = rc.Close()
				if upstream.IsLoopGuardError(streamErr) {
					// The stream was stopped before final usage could be established.
					// Preserve observed counts but do not label them as a complete bill.
					st.unreported = true
					st.status = http.StatusUnprocessableEntity
					if r.Context().Err() != nil {
						st.status = 499
					}
					if errors.As(streamErr, &loopErr) {
						log.Printf("WARN: [server] %s", loopErr.Message)
					}
					return
				}
				if _, contextFailure := upstream.ContextTooLongStreamDetail(streamErr); contextFailure {
					// Stream 已将失败发给客户端；这里只记失败并释放租约，不重复写入或解绑。
					st.status = http.StatusBadRequest
					releaseHeld()
					return
				}
				if r.Context().Err() != nil {
					st.status = 499
					fail(acct.UID)
					return
				}
				// 上游掐流（INTERNAL_ERROR / 连接中断）且客户端**还没收到任何内容**时换号重试：
				// 这是可恢复的偶发上游故障，重发通常直接成功。已交付过内容则不能重试——
				// 客户端会看到重复输出，只能如实报错。
				if upstream.IsUpstreamReadError(streamErr) && !deliveredContent(w) && readRotate < maxReadRotations {
					readRotate++
					log.Printf("WARN: [server] upstream stream cut before any content uid=%s model=%s — rotating to another account (attempt %d/%d)",
						logfmt.UID8(acct.UID), bareModel, readRotate, maxReadRotations)
					// fail 而非 releaseHeld：必须解绑会话粘性，否则下一轮又选中同一个号。
					fail(acct.UID)
					tried[acct.UID] = true
					// 关闭本轮上游 body 再重发（与轮转循环出口的 rc.Close 同理，避免 fd 堆积）。
					// stats 无需手工重建：它由每轮循环开头的 newChatStatsReaderSince(rc, …)
					// 新建，因此上一轮已入账的用量不会被重复累加。
					_ = rc.Close()
					// Responses 的写入器贯穿整个轮转循环，必须清掉上一轮的内容累积，
					// 否则重试的内容接在旧状态后面（客户端已收到的只是信封事件）。
					resetForRetry(w)
					continue
				}
				st.status = http.StatusBadGateway
				log.Printf("WARN: [server] stream incomplete uid=%s model=%s error=%v", logfmt.UID8(acct.UID), bareModel, streamErr)
				fail(acct.UID)
				return
			}
			h.cfg.Pool.NoteSuccess(acct.UID)
			h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
			flagReviewRejectedPeers(h.cfg.Pool, reviewRejectedUIDs, acct.UID, bareModel)
			// 绑定用 stickyKey（含无显式会话标识客户端的正文回退键）。
			if stickyKey != "" && h.cfg.Session != nil {
				h.cfg.Session.Bind(stickyKey, acct.UID)
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if credit, ok := stats.Credit(); ok && stats.CompleteUsage() {
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
			} else if _, hasCredit := stats.Credit(); !hasCredit && stats.hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit uid=%s model=%s (no cost observation)", logfmt.UID8(acct.UID), bareModel)
			}
			st.failed = false
			// 上一轮的流已经读完或已放弃，关闭失败不改变本轮结论。
			_ = rc.Close()
			return
		}
		// 非流式同理：Aggregate 只在**读完**之后才返回，出错时客户端一个字节都没收到，
		// 因此循环命中也能在同一账号上重发，用户同样无感。
		var resp map[string]any
		var err error
		var loopErr *upstream.StreamError
		for {
			resp, err = upstream.Aggregate(stats, streamOptions)
			if err == nil || !upstream.IsLoopGuardError(err) ||
				!errors.As(err, &loopErr) || loopRetries >= maxReasoningLoopRetries ||
				stopOnly ||
				r.Context().Err() != nil {
				break
			}
			// 同流式：被丢弃的那一轮上游已经产生并计费，用量照实累计。
			st.attemptError = traceStreamCode(err, r.Context())
			st.absorbUsage(stats)
			// 上一轮的流已经读完或已放弃，关闭失败不改变本轮结论。
			_ = rc.Close()
			loopRetries++
			log.Printf("INFO: [server] %s uid=%s model=%s — retrying same account (attempt %d/%d)",
				loopErr.Message, logfmt.UID8(acct.UID), bareModel, loopRetries, maxReasoningLoopRetries)
			// 与流式一致：保留重发时才出现的上下文错误与它实际报告的用量。
			rc, status, respBody, terr = h.cfg.Upstream.ChatStreamContext(chatContext, acct, body, clientIP, chatMeta)
			if detail, contextFailure := upstream.ContextTooLongHTTPDetail(status, respBody); terr == nil && contextFailure {
				st.absorbJSONUsage(respBody)
				st.unreported = true
				writeContextFailure(detail)
				return
			}
			if terr != nil || status >= 400 {
				// 重发没能建立：把失败如实回报，不能静默结束。
				retryErr := st.absorbRetryFailure(r.Context(), status, respBody, terr)
				st.unreported = true
				st.status = http.StatusBadGateway
				writeOpenAIError(w, st.status, retryErr.Code, retryErr.Message)
				if r.Context().Err() != nil {
					st.status = 499
				}
				return
			}
			stats = newChatStatsReaderSince(rc, st.start)
		}
		if err != nil {
			st.attemptError = traceStreamCode(err, r.Context())
		}
		st.absorbUsage(stats)
		// 上一轮的流已经读完或已放弃，关闭失败不改变本轮结论。
		_ = rc.Close()
		if err != nil {
			if upstream.IsLoopGuardError(err) {
				st.unreported = true
				st.status = http.StatusUnprocessableEntity
				writeOpenAIError(w, st.status, loopErrorCode(err), err.Error())
				return
			}
			if detail, contextFailure := upstream.ContextTooLongStreamDetail(err); contextFailure {
				writeContextFailure(detail)
				return
			}
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		if checker, ok := w.(interface{ ValidateCompletion(map[string]any) error }); ok {
			if err := checker.ValidateCompletion(resp); err != nil {
				st.trace.markLastFailure("response_contract_violation", false)
				writeOpenAIError(w, http.StatusBadGateway, "response_contract_violation", err.Error())
				st.status = http.StatusBadGateway
				return
			}
		}
		if formatter, ok := w.(interface{ PrepareCompletion(map[string]any) }); ok {
			formatter.PrepareCompletion(resp)
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		flagReviewRejectedPeers(h.cfg.Pool, reviewRejectedUIDs, acct.UID, bareModel)
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
		if err := writeJSON(w, http.StatusOK, resp); err != nil {
			st.status = http.StatusBadGateway
			return
		}
		st.status = http.StatusOK
		st.failed = false
		// 与流式共用原始观测，展示层的响应转换不能改变选号的成本分母。
		if credit, ok := stats.Credit(); ok && stats.CompleteUsage() {
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
		}
	}
	// 已经开始的流里不能再写 JSON 错误体：客户端会把它当成 SSE 事件解析，得到
	// 半截响应且没有终态事件（2026-09-30 审计发现）。流内交付失败交给适配器，
	// 它会按各自协议发出终止事件（Responses 的 response.failed、Anthropic 的
	// error 事件、Chat 的 error 帧 + [DONE]）。
	if !deliverStreamFailure(w, code, msg) {
		writeOpenAIError(w, status, code, msg)
	}
	st.status = status
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：内容拦截会立即 400 返回，其余种类 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不施冷却/熔断；但会累计「连续审核拒绝」计数，达到阈值
//     （或同请求别的号成功的硬证据）时按账号级标记停用（见 pool.NoteContentBlocked
//     与 flagReviewRejectedPeers）。停用发生在 handler 分支内，不经过本函数。
//   - ErrBadParams → 不罚账号，直接回 400 并保留脱敏后的诊断。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - ErrClient 在调用前直接回 400；其他未知错误不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, resetAt, "429 rate limit")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrSessionDead:
		// 走连续计数而非直接禁用：12153 会被临时性触发（网络抖动/上游闪断/refresh
		// 竞态），一次即永久杀号会误杀健康号——pool.NoteSessionDead 的注释记录了
		// 13 个被误停用账号的先例。此前这里直接调 Disable，把该保护整个绕过去了
		// （2026-10-02 第二轮体检发现：Classify 的 12153 子串匹配 + 这里的无条件
		// Disable 叠加，使一次误判就永久停号）。连续 sessionDeadThreshold 次才禁用。
		h.cfg.Pool.NoteSessionDead(uid)
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录）。**连续**达到 accountFaultThreshold 才硬禁用
		//     （NoteAccountFault）——上游也会用同一条码回内容审核拒绝，单次即杀会误伤
		//     健康号（2026-09-27 实测两个号因此被停用）。/status 以 disabled +
		//     disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			// 连续计数达到阈值才禁用（与 12153 同款保护）：上游会把「内容未通过安全
			// 审核」也回成 11140 + "request illegal"，一次 403 就永久杀号会误杀健康号。
			h.cfg.Pool.NoteAccountFault(uid)
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContextTooLong:
		// 上下文超限：请求体问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// 轮转循环已在该 kind 上直接 return，不消耗其他账号。
	case upstream.ErrContentBlocked:
		// 内容策略拦截：不施冷却/熔断（无 Cooldown/NoteError）。审核拒绝的账号维度
		// 判定（连续计数与「同请求别的号成功」硬证据）在 chatCompletions 的
		// ErrContentBlocked 分支完成，因此这里不做任何账号处置。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// loopErrorCode 取出重复短行保护的稳定错误码：推理侧与正文侧各自有码，调用方
// 据此区分「模型在思考里打转」和「重复正文已经发出去」。取不到时回落到推理侧码，
// 保持既有调用方兼容。
func loopErrorCode(err error) string {
	var streamErr *upstream.StreamError
	if errors.As(err, &streamErr) && streamErr.Code != "" {
		return streamErr.Code
	}
	return upstream.ReasoningLoopErrorCode
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(raw)
	if err == nil {
		err = flushHTTPResponse(w)
	}
	return err
}

// streamFailureWriter 由「已经开始流」的适配器实现：把失败交付在流内而不是 JSON。
type streamFailureWriter interface {
	StreamFailure(code, message string) bool
}

// deliveredContentWriter 由流式适配器实现：报告是否已经把**客户端可见的内容**
// 推下去了（正文、思考、工具调用参数等），而不是只有 ping 之类的保活帧。
//
// 用途：上游掐流（upstream_read_error）后想换号重试，但只有「客户端还什么都没收到」
// 时才安全——已经推过内容再重试，客户端会看到重复或矛盾的两段输出。
// 保活 ping 不算内容，重试不会造成重复。
type deliveredContentWriter interface {
	DeliveredContent() bool
}

// retryResetWriter 由「内容状态需要跨重发重置」的适配器实现。
// Responses 的写入器在构造时就确定、贯穿整个轮转循环，重发前必须清空上一轮的
// 内容累积，否则重试的内容无法正确交付（2026-09-30 审计发现）。
type retryResetWriter interface {
	ResetForRetry()
}

// resetForRetry 沿写入器链调用一次重置；没有适配器的路径（Chat 原生、Anthropic）
// 由各自的重发逻辑保证状态干净，这里静默跳过。
func resetForRetry(w http.ResponseWriter) {
	for current := w; current != nil; {
		if reset, ok := current.(retryResetWriter); ok {
			reset.ResetForRetry()
			return
		}
		next, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		current = next.Unwrap()
	}
}

// deliveredContent 沿写入器链询问是否已交付内容；没有适配器时保守返回 true
// （宁可放弃重试，也不要冒着重复输出的风险）。
func deliveredContent(w http.ResponseWriter) bool {
	for current := w; current != nil; {
		if probe, ok := current.(deliveredContentWriter); ok {
			return probe.DeliveredContent()
		}
		next, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		current = next.Unwrap()
	}
	return true
}

// deliverStreamFailure 沿写入器链找一个能把失败写进流的适配器；返回是否已交付。
func deliverStreamFailure(w http.ResponseWriter, code, message string) bool {
	for current := w; current != nil; {
		if failure, ok := current.(streamFailureWriter); ok && failure.StreamFailure(code, message) {
			return true
		}
		next, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		current = next.Unwrap()
	}
	return false
}

// holdProgress 沿响应写入器链找到支持 HoldProgress 的适配器并触发一次；
// 没有适配器（原生 Chat 无契约时）静默跳过。
func holdProgress(w http.ResponseWriter) {
	for current := w; current != nil; {
		if progress, ok := current.(interface{ HoldProgress() }); ok {
			progress.HoldProgress()
			return
		}
		next, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		current = next.Unwrap()
	}
}

// writeOpenAIError 写一个 OpenAI 形状的错误响应体。
//
// 流已经开始时必须走适配器把失败交付在流内：往已开的 SSE 流里写 JSON 错误体
// （无 data: 前缀、无空行分隔）会让客户端解析失败，而且流里没有任何终态事件，
// 客户端只能等到超时。适配器（Responses 的 response.failed、Anthropic 的
// error 事件、Chat 的 error 帧 + [DONE]）各自按协议收尾；没有适配器时才退回
// 原始写法——那是尚未开流、或原生 Chat 无契约的路径。
//
// 放在这里而不是各个调用点：只要写入器已经开始流，任何调用点都不能再直写 JSON
// （2026-09-30 深度体检发现：换号重试没能建立时正是从这条路径漏出去的）。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	observeResponseError(w, code)
	if deliverStreamFailure(w, code, msg) {
		return
	}
	_ = writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openAIErrorType(status),
			"code":    code,
		},
	})
}

// openAIErrorType 按状态码给出 OpenAI 的错误类型。SDK 会按 error.type 分类处理
// （重试、提示鉴权失败、判定请求错误），一律 api_error 会让它们无法区分。
func openAIErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusUnsupportedMediaType:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "server_error"
	}
	return "api_error"
}

// flagReviewRejectedPeers 用硬证据停用被内容审核拒绝的账号：本次客户端请求里
// rejected 中的账号被上游 11140 审核拒绝过，而 succeededUID 用**同一份正文**成功返回。
// 内容在别的号上能过，说明拒绝属于账号维度，立即停用，不再等它被反复选中。
//
// 只在请求内复用同一 body（换号重试）时成立，这正是调用点所在的位置；succeededUID
// 自身不在 rejected 里（它按定义成功了），无需排除。
func flagReviewRejectedPeers(p *pool.Pool, rejected []string, succeededUID, model string) {
	for _, uid := range rejected {
		if uid == succeededUID {
			continue
		}
		if p.FlagReviewAccount(uid) {
			log.Printf("WARN: [server] content review rejection — disabling account "+
				"uid=%s reason=peer-succeeded (account-level flag) model=%s", logfmt.UID8(uid), model)
		}
	}
}
