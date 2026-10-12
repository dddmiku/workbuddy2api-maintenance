// fission.go — Trae「邀请新用户得积分」（code fission）的发奖调用。
//
// ═══ 为什么发奖要网关主动调 ═══
//
// 面板上邀请记录长期停在「邀请中」，不是服务端没发，而是**发奖动作在客户端本地**：
// TRAE 桌面端每次登录时会自己调一次
//
//	POST {ugApi}/trae/api/v2/ug/code_fission/grant
//	body: {"activity_id":"code_fission_202609"}
//
// 服务端只登记「谁邀请了谁」，积分要等被邀请人那次客户端登录上报才落袋。
// 被邀请人没登过桌面端（或登录时用的设备已被判重），记录就永远挂着。
//
// 本文件把那一次调用搬到服务端：形态与客户端 out-build 里的实现逐字对齐
// （CommercialActivity.hb）——同样的端点、同样的 body、同样的头族。
// 2026-10-12 实测：调用前 4500 → 调用后 5000，新增「TraeCode 邀请奖励」500 分包。
//
// ═══ 设备号 ═══
//
// 活动规则里「同一台电脑只能被邀请一次」落到实现上就是 x-device-id。客户端的
// x-device-id 是 guaranteedDeviceId（本机固定），所以同一台机器重复登录不会重复发奖。
// 服务端补发时要能指定设备号：gen 沿用签到那套派生（见 checkinDeviceIDGen），
// gen=0 是账号默认设备号，gen>0 是全新设备。
package trae

import (
	"context"
	"errors"
	"fmt"

	"workbuddy2api/internal/auth"
)

const (
	// FissionActivityID 是 code fission（邀请新用户）活动的活动号。
	//
	// 注意它与面板邀请码（share_link 返回的 8 位码）不是一个东西：前者是活动标识，
	// 后者是每个账号各自的邀请码。客户端把它硬编码在 handleCommercialActivity 的
	// 分支判断里（`e==="code_fission_202609"`），我们照抄。
	FissionActivityID = "code_fission_202609"

	fissionGrantPath     = "/trae/api/v2/ug/code_fission/grant"
	fissionShareLinkPath = "/trae/api/v2/ug/code_fission/share_link"
)

// 上游对 code fission 的业务码。都是 HTTP 200 + 业务码表达结果（客户端也是这么判的：
// 先看 status 再看 code，code 非 0 即失败）。
const (
	// FissionCodeNotInvited 该账号没有被邀请过（或邀请关系尚未成立），无奖可发。
	FissionCodeNotInvited = 1027
	// FissionCodeAlreadyGranted 已经发过了——重复调用是幂等的成功，不是错误。
	FissionCodeAlreadyGranted = 1028
	// FissionCodeActivityUnavailable 活动未开始/已结束/账号不符合参与资格。
	FissionCodeActivityUnavailable = 9090
	// FissionCodeDeviceUsed 该设备号已被用于一次邀请（活动规则「同一台电脑只能
	// 被邀请一次」）。调用方应换一个 gen 重试。
	FissionCodeDeviceUsed = 9091
	// FissionCodeNoBenefit 暂无可领取权益——上游判定这次调用没有可发的奖励。
	//
	// 2026-10-12 实测：邀请方账号（码 84LLUK39UV97）调用返回 9070，而同期被邀请方
	// 账号发放成功。合理解释是**奖励按「被邀请人那次客户端登录」发放，且只发一次**：
	// 受邀方领过之后，邀请方侧再单独调用就没有可领项了（邀请方那 500 随受邀方的
	// 登录一并结算）。换设备号重试无意义，属正常的「无奖可发」，不是故障。
	FissionCodeNoBenefit = 9070
)

// FissionResult 是一次发奖调用的结果。
type FissionResult struct {
	// Granted 本次是否真的发出了积分（code 0）。
	Granted bool
	// Already 是否「此前已发过」（code 1028）。调用方应把它当成功处理：
	// 奖励已在账上，重复调用不会重复发放。
	Already bool
	// Code 上游业务码（0 表示成功）。
	Code float64
	// Message 上游文案（原样保留，便于排障；不要拿它做分支判断）。
	Message string
}

// GrantCodeFission 触发一次邀请奖励发放。
//
// 语义：
//   - code 0    → Granted=true，积分已到账；
//   - code 1028 → Already=true，此前已发过（幂等成功，不重复发放）；
//   - code 1027 → 返回错误：该账号没有被邀请的记录，无奖可发；
//   - code 9090 → 返回错误：活动不可用/账号不符合资格；
//   - code 9091 → 返回 *FissionDeviceUsedError：设备号被判重，调用方换 gen 重试。
//
// gen 是设备号代数（见 checkinDeviceIDGen）：gen=0 用账号默认设备号，gen>0 是
// 全新设备。要绕过「同一台电脑只能被邀请一次」就传 gen>0。
func GrantCodeFission(ctx context.Context, a *auth.Auth, gen int) (FissionResult, error) {
	data, err := postTraeAPI(ctx, a, fissionGrantPath,
		map[string]any{"activity_id": FissionActivityID}, gen)
	if err != nil {
		return FissionResult{}, err
	}
	return classifyFissionResponse(data)
}

// classifyFissionResponse 把上游响应对象判成 FissionResult 或具名错误。
//
// 抽出来单独可测：判定分支（成功/已发/无邀请/活动不可用/设备判重/未知码）
// 是这块最容易写错、也最需要被回归锁住的部分，不该埋在网络调用里。
func classifyFissionResponse(data map[string]any) (FissionResult, error) {
	message, _ := data["message"].(string)
	code, hasCode := asFloat(data["code"])
	if !hasCode {
		// 没有 code 字段：上游改了响应形态。如实上报，不猜成功。
		return FissionResult{}, fmt.Errorf("trae fission grant: 响应缺少 code 字段: %s",
			truncate(fmt.Sprint(data), 200))
	}
	switch code {
	case 0:
		return FissionResult{Granted: true, Code: code, Message: message}, nil
	case FissionCodeAlreadyGranted:
		return FissionResult{Already: true, Code: code, Message: message}, nil
	case FissionCodeDeviceUsed:
		return FissionResult{Code: code, Message: message}, &FissionDeviceUsedError{Message: message}
	case FissionCodeNoBenefit:
		return FissionResult{Code: code, Message: message},
			fmt.Errorf("trae fission grant: 暂无可领取权益（code %g）: %s", code, message)
	case FissionCodeNotInvited:
		return FissionResult{Code: code, Message: message},
			fmt.Errorf("trae fission grant: 该账号没有被邀请的记录（code %g）: %s", code, message)
	case FissionCodeActivityUnavailable:
		return FissionResult{Code: code, Message: message},
			fmt.Errorf("trae fission grant: 活动不可用或账号不符合资格（code %g）: %s", code, message)
	default:
		return FissionResult{Code: code, Message: message},
			fmt.Errorf("trae fission grant: code %g: %s", code, message)
	}
}

// FissionDeviceUsedError 是 9091（设备号被判重）的具名错误。
//
// 与签到的 9074 同样是**设备维度**的信号：调用方换一个 gen 即可绕过。
// 用具名类型而不是匹配文案，理由同 RateLimitError——上游 message 会随改版漂移。
type FissionDeviceUsedError struct {
	Message string
}

func (e *FissionDeviceUsedError) Error() string {
	return fmt.Sprintf("trae fission grant: 该设备号已被用于一次邀请（code %d），换设备号重试: %s",
		FissionCodeDeviceUsed, e.Message)
}

// IsFissionDeviceUsed 报告错误是否为设备号判重（9091）。
func IsFissionDeviceUsed(err error) bool {
	var target *FissionDeviceUsedError
	return errors.As(err, &target)
}

// FissionShareLink 查询账号自己的邀请码。
//
// 与发奖不同，这个接口返回的是**邀请人**的码（面板上「我的邀请链接」那串）。
// 用来核对某条邀请记录属于哪个账号：记录里的邀请方 uid ↔ 这里的码。
func FissionShareLink(ctx context.Context, a *auth.Auth) (string, error) {
	data, err := postTraeAPI(ctx, a, fissionShareLinkPath,
		map[string]any{"activity_id": FissionActivityID}, 0)
	if err != nil {
		return "", err
	}
	if code, ok := asFloat(data["code"]); ok && code != 0 {
		message, _ := data["message"].(string)
		return "", fmt.Errorf("trae fission share_link: code %g: %s", code, message)
	}
	code, _ := data["invite_code"].(string)
	return code, nil
}
