// ═══ 更新日志 ═══
// 2026-10-13：初版——/api/state 的数据组装（与 panel/app.py 的 build_state 同口径）。
//
// 这个接口是面板首屏的唯一数据源：账号列表（含文件层的启停状态 + 账号池的冷却/熔断
// 台账 + 积分）与全局池统计。三份数据来自三个不同的来源，必须在这里对齐：
//
//   - 账号文件（本机 auths 目录）——事实来源是「有哪些账号、文件是否被停用」；
//   - 网关 /status——事实来源是「账号池认为它现在能不能用」；
//   - ./credit——事实来源是「它还剩多少积分」。
//
// 三者是**独立**的状态，不能互相推导：文件启用不代表池里没被停用（上游封禁是池级
// 状态），池里健康不代表积分没到期。面板把它们并排显示，让运维看到全貌。
package panel

import (
	"context"
	"strings"
)

// PoolState 是账号在网关账号池里的状态（对应 /status 的 accounts[] 条目）。
type PoolState struct {
	InPool         bool   `json:"inPool"`
	Healthy        bool   `json:"healthy"`
	Cooling        bool   `json:"cooling"`
	Until          any    `json:"until"`
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabledReason"`
	InFlight       any    `json:"inFlight"`
	BreakerFails   any    `json:"breakerFails"`
	LastSuccess    any    `json:"lastSuccess"`
	LastErr        any    `json:"lastErr"`
	// 冷却台账：面板据此显示「为什么冷却、还剩多久」，而不是把凭证有效期的
	// 「剩 N 天」摆在「冷却中」旁边（用户反馈：看起来像冷却要等 363 天）。
	CoolRemaining any    `json:"coolRemaining"`
	CoolKind      string `json:"coolKind"`
	Reason        string `json:"reason"`
	SuccessCount  any    `json:"successCount"`
	ErrTotal      any    `json:"errTotal"`
}

// CreditState 是账号的积分信息。
type CreditState struct {
	// Supported 恒为 true：WorkBuddy 走 billing/meter、Trae 走 pay API，两者都能查
	// （./credit 按域分流）。这里保留字段是为了前端不必改结构。
	Supported           bool  `json:"supported"`
	Remain              any   `json:"remain"`
	Size                any   `json:"size"`
	Used                any   `json:"used"`
	Packages            any   `json:"packages"`
	OK                  any   `json:"ok"`
	Error               any   `json:"error"`
	Expiring            int64 `json:"expiring"`
	Expiry              any   `json:"expiry"`
	ExpiringWindowHours int64 `json:"expiringWindowHours"`
}

// AccountState 是面板账号行（auth_summary + pool + credits）。
type AccountState struct {
	AuthSummary
	Pool    PoolState   `json:"pool"`
	Credits CreditState `json:"credits"`
}

// PoolTotals 是全局池统计（直接透传网关 /status 的 pool 段）。
type PoolTotals struct {
	Total          any `json:"total"`
	Healthy        any `json:"healthy"`
	Cooling        any `json:"cooling"`
	Disabled       any `json:"disabled"`
	InFlightFull   any `json:"inFlightFull"`
	RealmTotals    any `json:"realmTotals"`
	StickySessions any `json:"stickySessions"`
}

// StatePayload 是 /api/state 的响应体。
type StatePayload struct {
	Service        string         `json:"service"`
	Version        string         `json:"version"`
	PanelVersion   string         `json:"panelVersion"`
	ContainerRun   bool           `json:"containerRunning"`
	Accounts       []AccountState `json:"accounts"`
	Pool           PoolTotals     `json:"pool"`
	CreditTotals   any            `json:"creditTotals"`
	CreditError    any            `json:"creditError"`
	CreditPending  bool           `json:"creditPending"`
	PendingRestart bool           `json:"pendingRestart"`
	// APIKey 恒为空串：面板**不**把调用密钥回传给前端（它是可分发的凭证，
	// 回显等于把它发给任何能打开面板的人）。保留字段是为了前端不必改结构。
	APIKey         string `json:"apiKey"`
	APIKeysManaged bool   `json:"apiKeysManaged"`
}

// PanelVersion 是面板自身的版本号。报给前端的 version 字段用**网关**版本
// （面板随网关一起打包，用户看到的应该是网关版本），本值另用 panelVersion 字段，
// 两者不一致时前端可同时显示——2026-10-10 实测：网关已到 v2.6.19、面板仍显示
// v2.4.17，用户以为没更新成功。
const PanelVersion = "2.5.0"

// StateBuilder 组装 /api/state。
type StateBuilder struct {
	Store     *AccountStore
	Gateway   *GatewayClient
	Credits   *CreditCache
	Runner    Runner
	Container string
}

// Build 生成首屏状态。forceCredit=true 时等待一次新积分查询。
func (b *StateBuilder) Build(ctx context.Context, forceCredit bool) StatePayload {
	pool := b.Gateway.Get(ctx, "/status")
	poolByUID := map[string]map[string]any{}
	if accounts, ok := pool["accounts"].([]any); ok {
		for _, item := range accounts {
			account, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if uid := jsonString(account["uid"]); uid != "" {
				poolByUID[uid] = account
			}
		}
	}
	// 网关版本从 /healthz 取，而不是复用面板自己的 PanelVersion：面板是随网关一起
	// 打包的，但两个版本号各自维护，长期不动的那个必然漂移。
	health := b.Gateway.Get(ctx, "/healthz")
	gatewayVersion := jsonString(health["version"])

	creditRaw := b.Credits.Get(ctx, forceCredit)
	credByUID, _ := CreditsByUID(creditRaw)
	creditPending := jsonBool(creditRaw["pending"], false)

	accounts := []AccountState{}
	for _, entry := range b.Store.List() {
		summary := Summary(entry)
		poolEntry := poolByUID[summary.UID]
		creditEntry := credByUID[summary.UID]
		row := AccountState{AuthSummary: summary}
		row.Pool = poolStateFrom(poolEntry)
		row.Credits = creditStateFrom(creditEntry, poolEntry)
		accounts = append(accounts, row)
	}

	// 「有文件被停用、但账号还在池里」= 改动尚未生效，提示用户重启。
	changed := []string{}
	for _, account := range accounts {
		if account.Disabled && account.Pool.InPool {
			changed = append(changed, account.UID)
		}
	}

	return StatePayload{
		Service:        "wb2api-admin",
		Version:        firstNonEmpty(gatewayVersion, PanelVersion),
		PanelVersion:   PanelVersion,
		ContainerRun:   b.containerRunning(ctx),
		Accounts:       accounts,
		Pool:           poolTotalsFrom(pool),
		CreditTotals:   creditRaw["total"],
		CreditError:    creditRaw["error"],
		CreditPending:  creditPending,
		PendingRestart: len(changed) > 0,
		APIKey:         "",
		APIKeysManaged: b.Gateway.Management != nil && b.Gateway.Management.ManagementEnabled(),
	}
}

func (b *StateBuilder) containerRunning(ctx context.Context) bool {
	if b.Runner == nil {
		return false
	}
	container := b.Container
	if container == "" {
		container = "workbuddy2api"
	}
	result := b.Runner.Run(ctx, []string{"inspect", "-f", "{{.State.Running}}", container}, execShortTimeout, nil)
	return result.Code == 0 && strings.TrimSpace(result.Stdout) == "true"
}

func poolStateFrom(entry map[string]any) PoolState {
	state := PoolState{
		InPool:         len(entry) > 0,
		Cooling:        jsonBool(entry["cooling"], false),
		Disabled:       jsonBool(entry["disabled"], false),
		DisabledReason: jsonString(entry["disabled_reason"]),
		Until:          entry["until"],
		InFlight:       orZero(entry["in_flight"]),
		BreakerFails:   orZero(entry["breaker_fails"]),
		LastSuccess:    entry["last_success"],
		LastErr:        entry["last_err"],
		CoolRemaining:  orZero(entry["cool_remaining_sec"]),
		CoolKind:       jsonString(entry["cool_kind"]),
		Reason:         jsonString(entry["reason"]),
		SuccessCount:   orZero(entry["success_count"]),
		ErrTotal:       orZero(entry["err_total"]),
	}
	// healthy = 在池里 且 没冷却 且 没被池级停用。三个条件都要看：只看 inPool
	// 会把「上游封禁但还在池里」显示成健康，只看 cooling 会漏掉停用。
	state.Healthy = state.InPool && !state.Cooling && !state.Disabled
	return state
}

func creditStateFrom(credit, pool map[string]any) CreditState {
	// 快过期积分子集（remain 的一部分）。来源优先级：
	//   1) ./credit 的 expiring（本次查询时按 WB2A_EXPIRING_SOON 现算，全账号都有）；
	//   2) 网关 /status 的 credits_expiring（签到任务写的，只覆盖 CN 号且最长滞后 12h）。
	// 取较大者：两者口径同源（同一上游字段、同一窗口），差异只来自数据新鲜度；
	// 取大不会漏报「要作废了」，符合这个提示的用途（宁可多提醒，不可漏）。
	expiring := jsonInt64(credit["expiring"])
	if fromPool := jsonInt64(pool["credits_expiring"]); fromPool > expiring {
		expiring = fromPool
	}
	return CreditState{
		Supported:           true,
		Remain:              credit["remain"],
		Size:                credit["size"],
		Used:                credit["used"],
		Packages:            credit["packages"],
		OK:                  credit["ok"],
		Error:               credit["error"],
		Expiring:            expiring,
		Expiry:              credit["expiry"],
		ExpiringWindowHours: jsonInt64(credit["expiring_window_hours"]),
	}
}

func poolTotalsFrom(pool map[string]any) PoolTotals {
	section := jsonObject(pool["pool"])
	return PoolTotals{
		Total:          section["total"],
		Healthy:        section["healthy"],
		Cooling:        section["cooling"],
		Disabled:       section["disabled"],
		InFlightFull:   section["in_flight_full"],
		RealmTotals:    section["realm_totals"],
		StickySessions: section["sticky_sessions"],
	}
}

// orZero 把缺失的数值字段补成 0（前端会直接做算术，undefined 会变成 NaN）。
func orZero(value any) any {
	if value == nil {
		return 0
	}
	return value
}

// jsonInt64 取整数值；取不到返回 0。
func jsonInt64(value any) int64 {
	number, ok := jsonNumber(value)
	if !ok {
		return 0
	}
	return int64(number)
}
