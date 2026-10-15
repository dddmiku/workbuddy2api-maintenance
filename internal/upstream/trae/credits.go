// credits.go — Trae 域的签到与积分查询。
//
// 与 WorkBuddy 完全不同的一套端点（都在 api.trae.cn 的 UG / pay API 上）：
//
//	签到状态  POST /trae/api/v2/ug/checkin_credits/status
//	签到领取  POST /trae/api/v2/ug/checkin_credits/claim
//	账号积分  POST /trae/api/v2/pay/ide_user_ent_usage
//
// 鉴权用同一枚 Cloud-IDE-JWT，但头族是签到专用的一套（x-device-id 必须存在，
// 上游按设备维度做每日签到去重）。
//
// ═══ 更新日志 ═══
// 2026-10-11：初版——签到状态/领取与账号积分，端点来自 Trae2api-cn 实测。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// Trae API host（签到与积分都在这台，不是模型网关）。
//
// 是 var 而非 const：测试用 httptest 临时改写它，覆盖请求组装与响应判定的
// 完整链路（改回原值由测试的 cleanup 负责）。生产代码从不写它。
var TraeAPIHost = "https://api.trae.cn"

const (
	checkinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	checkinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
	entUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// checkinDeviceBrand/Type 签到头里的设备描述（对齐 Trae 桌面端形态）。
const (
	checkinDeviceBrand = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	checkinDeviceType  = "windows"
)

// CheckinStatus 是签到状态。
type CheckinStatus struct {
	// Enabled 上游是否开放签到。
	Enabled bool
	// CheckedIn 今天是否已签到。
	CheckedIn bool
	// Credits 本次可领/已领的积分。
	Credits float64
	// ExtraCredits 额外奖励积分。
	ExtraCredits float64
	// Message 上游文案。
	Message string
}

// Credits 是账号积分概览。
type Credits struct {
	// Total 总额度。
	Total float64
	// Consumed 已消费。
	Consumed float64
	// Remaining 剩余（Total - Consumed，上游没给时由两者算出）。
	Remaining float64
	// Packs 逐包明细（到期时间 + 额度），供面板显示有效期。
	Packs []CreditPack
}

// CreditPack 一个积分包。
type CreditPack struct {
	Name      string
	Limit     float64
	Remaining float64
	// EndTime Unix 秒；0 表示无到期时间。
	EndTime int64
}

// checkinHeaders 构造签到/积分接口的头族。
//
// 与 llm_utils_chat 的头族不同：这里只需要 JWT + 设备标识（上游按设备维度做
// 每日签到去重，x-device-id 缺失会被判成新设备）。设备指纹按 UID 派生，跨请求稳定。
//
// gen 是设备号「代数」：9074 是**设备维度**的限流，换一个全新派生的设备号即可
// 立刻签上（见 ClaimCheckin 的注释）。gen=0 是默认设备号，命中 9074 后调用方
// 传 gen+1 重试一次。
func checkinHeaders(a *auth.Auth, gen int) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	h.Set("x-device-id", checkinDeviceIDGen(a.UID, gen))
	h.Set("x-device-brand", checkinDeviceBrand)
	h.Set("x-device-type", checkinDeviceType)
	return h
}

// checkinDeviceID 默认设备号（gen=0）。
func checkinDeviceID(uid string) string {
	return checkinDeviceIDGen(uid, 0)
}

// checkinDeviceIDGen 派生第 gen 代设备号。
//
// gen=0 用原材料的 uid（保持与历史一致，已在用的设备号不因这次改动而改变——
// 改了会让所有账号在签到侧变成「新设备」，白白丢掉上游的设备维度状态）；
// gen>0 掺入代数，得到与第 0 代无关的新设备号。
func checkinDeviceIDGen(uid string, gen int) string {
	material := uid
	if gen > 0 {
		material = fmt.Sprintf("%s#gen%d", uid, gen)
	}
	sum := deviceID(material, "checkin")
	// 取十六进制前缀转十进制并截到 16 位：形态与真实客户端一致（纯数字）。
	var value uint64
	for i := 0; i < 16 && i < len(sum); i++ {
		digit := sum[i]
		if digit >= 'a' {
			value = value*16 + uint64(digit-'a'+10)
		} else {
			value = value*16 + uint64(digit-'0')
		}
	}
	return fmt.Sprintf("%016d", value%10000000000000000)
}

// postTraeAPI 向 Trae API 发一个 POST，返回解析后的对象。
func postTraeAPI(ctx context.Context, a *auth.Auth, path string, payload any, gen int) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeAPIHost+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range checkinHeaders(a, gen) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae api %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("trae api %s read: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trae api %s: HTTP %d: %s", path, resp.StatusCode, truncate(string(data), 300))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("trae api %s parse: %w", path, err)
	}
	return out, nil
}

// AuthFailedCode 上游在 HTTP 200 的业务码里表达「这枚 token 不认了」。
//
// 与 HTTP 401 不同：401 是传输层的鉴权失败（头族/签名问题，重试无意义），
// 1001 是**服务端主动作废了这枚 JWT**——换一枚新签发的 token 就能继续。
// 参考实现（autumnsentiment/Trae2api-cn 的 _checkin_auth_failed）同样只在
// 业务码层面判 1001，并配一句文案兜底 "not able to authenticate"。
const AuthFailedCode = 1001

// AuthError 是 1001 的具名错误，调用方据此触发「刷新凭据后重试一次」。
//
// 为什么必须具名而不是匹配文案：上游 message 是中文文案会随改版漂移，拿它做分支
// 判断是脆的（同 RateLimitError 的理由）。
type AuthError struct {
	Message string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("trae: token rejected by upstream (%d): %s", AuthFailedCode, e.Message)
}

// IsAuthFailure 报告错误是否为上游作废 token（1001 / 文案兜底）。
func IsAuthFailure(err error) bool {
	var target *AuthError
	if errors.As(err, &target) {
		return true
	}
	// 文案兜底：上游偶发只给 message 不给 code（实测形态见参考实现
	// _checkin_auth_failed 的同一处理）。判据限定为这句英文原文，不用宽泛的
	// "auth" 之类——宽判据会把正常错误误判成鉴权失败，触发无谓的刷新。
	return err != nil && strings.Contains(err.Error(), "not able to authenticate")
}

// businessCodeError 把非 0 业务码转成具名错误（1001/9074 有专用类型，其余保持通用）。
func businessCodeError(code float64, message, scope string) error {
	switch code {
	case RateLimitCode:
		return &RateLimitError{Message: message}
	case AuthFailedCode:
		return &AuthError{Message: message}
	}
	return fmt.Errorf("trae %s: code %g: %s", scope, code, message)
}

// CheckinStatusOf 查询签到状态。
//
// 字段校验对齐参考实现（_post_checkin 的 status 分支）：
//   - enable / checked_in **必须是布尔**。上游给 {"enable":1} 这类数字时直接报错，
//     不能靠 Go 的类型断言静默取到 false——那会让「签到开着」被误判成「签到关了」，
//     任务整轮跳过而用户毫无察觉。
//   - credits 是可选字段：非数值/非有限/<= 0 一律视为「本次没有可领积分」，置 0。
func CheckinStatusOf(ctx context.Context, a *auth.Auth) (CheckinStatus, error) {
	data, err := postTraeAPI(ctx, a, checkinStatusPath, nil, 0)
	if err != nil {
		return CheckinStatus{}, err
	}
	if code, ok := asFloat(data["code"]); ok && code != 0 {
		message, _ := data["message"].(string)
		return CheckinStatus{}, businessCodeError(code, message, "checkin status")
	}
	enabled, ok := data["enable"].(bool)
	if !ok {
		return CheckinStatus{}, fmt.Errorf("trae checkin status: enable 字段缺失或非布尔（上游响应形态变了）")
	}
	checkedIn, ok := data["checked_in"].(bool)
	if !ok {
		return CheckinStatus{}, fmt.Errorf("trae checkin status: checked_in 字段缺失或非布尔（上游响应形态变了）")
	}
	status := CheckinStatus{Enabled: enabled, CheckedIn: checkedIn}
	status.Message, _ = data["message"].(string)
	if v, ok := asFloat(data["credits"]); ok && v > 0 {
		status.Credits = v
	}
	if v, ok := asFloat(data["extra_credits"]); ok && v > 0 {
		status.ExtraCredits = v
	}
	return status, nil
}

// RateLimitCode 是上游对签到 claim 的限流业务码（"当前参与用户太多，请稍后再试"）。
//
// 它是**设备维度**的，不是账号维度：2026-10-11 实测同一账号（同一 JWT）在同一
// 分钟内，第 0 代设备号返回 9074，而紧接着用新派生的第 1、2 代设备号**立刻签到
// 成功**。所以正确的处理是「换设备号重试一次」，而不是放弃这个账号。
//
// 注意判据必须用**新派生**的设备号（checkinDeviceIDGen(uid, gen+1)），
// 单纯重复调用同一个设备号仍然会 9074。
const RateLimitCode = 9074

// RateLimitError 是 9074 的具名错误：调用方用 errors.As 判定，不必匹配字符串
// （上游 message 是中文文案，会随上游改版漂移，拿它做分支判断是脆的）。
type RateLimitError struct {
	Message string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("trae checkin: rate limited by upstream (%d): %s", RateLimitCode, e.Message)
}

// IsRateLimited 报告错误是否为上游签到限流（9074）。
func IsRateLimited(err error) bool {
	var target *RateLimitError
	return errors.As(err, &target)
}

// DeviceAlreadyCheckedCode 该设备号已被用于今日签到（**设备维度**，不是账号维度）。
//
// 语义要看清：文案是「该设备已签到」，但说的是**这个设备号**今天签过——可能是这个
// 账号签的，也可能是别的账号用同一设备号签的。所以**绝不能**据此判定本账号今天
// 已签到（参考实现 trae_client.py 的注释明确写了这一点，并有测试固化：
// `_checkin_claim_ok({"code": 9095, ...}) == False`）。
//
// 正确处置：当作「本次 claim 没有为这个账号拿到积分」，但**不冷却、不重试**——
// 换个设备号重试才是有效动作（换号会换掉 x-device-id）。
const DeviceAlreadyCheckedCode = 9095

// ClaimCheckin 领取今天的签到积分。
//
// 上游用业务码表达结果，不都是 HTTP 错误：
//   - code 0     成功。**实测（2026-10-15）claim 是幂等的**：今天已经签过的账号
//     再调返回的也是 code 0 + "success"，不是 9095。所以重复调用不会浪费当天机会，
//     重试是安全的（这是「自动重试循环」成立的前提）。
//   - code 9074  设备维度的限流。调用方应换一个**新派生**的设备号重试一次
//     （传 gen+1 再调本函数）；仍失败才退回有界退避。参考实现
//     （autumnsentiment/Trae2api-cn）的做法就是「轮换一次再重试」；
//   - code 9095  该设备号今日已签（设备维度，不证明本账号签过）→ DeviceCheckedError，
//     调用方应换设备号而不是判定「已签到」；
//   - code 1001  token 被上游作废 → AuthError，调用方刷新凭据后重试一次；
//   - 其它非 0   如实上报，不做"当成已签到"的猜测。
func ClaimCheckin(ctx context.Context, a *auth.Auth, gen int) (CheckinStatus, error) {
	data, err := postTraeAPI(ctx, a, checkinClaimPath, nil, gen)
	if err != nil {
		return CheckinStatus{}, err
	}
	message, _ := data["message"].(string)
	if code, ok := asFloat(data["code"]); ok && code != 0 {
		switch code {
		case RateLimitCode:
			return CheckinStatus{}, &RateLimitError{Message: message}
		case DeviceAlreadyCheckedCode:
			return CheckinStatus{}, &DeviceCheckedError{Message: message}
		case AuthFailedCode:
			return CheckinStatus{}, &AuthError{Message: message}
		}
		return CheckinStatus{}, fmt.Errorf("trae checkin: code %g: %s", code, message)
	}
	status := CheckinStatus{CheckedIn: true, Message: message}
	if v, ok := asFloat(data["credits"]); ok && v > 0 {
		status.Credits = v
	}
	if v, ok := asFloat(data["extra_credits"]); ok && v > 0 {
		status.ExtraCredits = v
	}
	return status, nil
}

// DeviceCheckedError 是 9095 的具名错误（该设备号今日已签，不代表本账号签过）。
type DeviceCheckedError struct {
	Message string
}

func (e *DeviceCheckedError) Error() string {
	return fmt.Sprintf("trae checkin: device already checked today (%d): %s", DeviceAlreadyCheckedCode, e.Message)
}

// IsDeviceAlreadyChecked 报告错误是否为 9095（设备号今日已签）。
func IsDeviceAlreadyChecked(err error) bool {
	var target *DeviceCheckedError
	return errors.As(err, &target)
}

// FetchCredits 查询账号积分与逐包明细。
//
// req_source=1 是通用 IDE 额度（客户端显示的那个总数）。
func FetchCredits(ctx context.Context, a *auth.Auth) (Credits, error) {
	data, err := postTraeAPI(ctx, a, entUsagePath,
		map[string]any{"require_usage": true, "req_source": 1}, 0)
	if err != nil {
		return Credits{}, err
	}
	if code, ok := asFloat(data["code"]); ok && code != 0 {
		message, _ := data["message"].(string)
		return Credits{}, fmt.Errorf("trae credits: code %g: %s", code, message)
	}
	out := Credits{}
	if summary, ok := data["usage_summary"].(map[string]any); ok {
		if v, ok := asFloat(summary["total_amount"]); ok {
			out.Total = v
		}
		if v, ok := asFloat(summary["consumed_amount"]); ok {
			out.Consumed = v
		}
	}
	out.Remaining = out.Total - out.Consumed
	if out.Remaining < 0 {
		out.Remaining = 0
	}
	if packs, ok := data["user_entitlement_pack_list"].([]any); ok {
		for _, raw := range packs {
			pack, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out.Packs = append(out.Packs, parseCreditPack(pack))
		}
	}
	return out, nil
}

// parseCreditPack 解析一个积分包（层级较深，逐层容错）。
func parseCreditPack(pack map[string]any) CreditPack {
	out := CreditPack{}
	base, _ := pack["entitlement_base_info"].(map[string]any)
	if base != nil {
		if v, ok := asFloat(base["end_time"]); ok && v > 0 {
			out.EndTime = int64(v)
		}
		if extra, ok := base["product_extra"].(map[string]any); ok {
			if pe, ok := extra["package_extra"].(map[string]any); ok {
				if name, ok := pe["package_name"].(string); ok {
					out.Name = name
				}
				if quota, ok := pe["quota"].(map[string]any); ok {
					if v, ok := asFloat(quota["credits_limit"]); ok {
						out.Limit = v
					}
				}
			}
		}
	}
	if v, ok := asFloat(pack["remaining_amount"]); ok {
		out.Remaining = v
	} else if v, ok := asFloat(pack["available_amount"]); ok {
		out.Remaining = v
	}
	if out.Name == "" {
		out.Name, _ = pack["display_desc"].(string)
	}
	return out
}

// IsTraeRealm 报告账号是否属于 Trae 域（供上层按域分流）。
func IsTraeRealm(a *auth.Auth) bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.RealmStored()), Realm)
}

// RawEntUsage 返回 ent_usage 接口的原始解析结果。
//
// 只用于排障：积分包的字段层级较深且上游会调整，`Credits` 只挑出网关需要的
// 那几个字段，字段改名时「解析出来是 0」和「上游真的返回 0」在结构化结果里
// 长得一样。看原始 JSON 才能确定是哪一种（2026-10-14 排查 trae 积分包
// remaining 恒为 0 时正是这个用途）。
func RawEntUsage(ctx context.Context, a *auth.Auth) (map[string]any, error) {
	return postTraeAPI(ctx, a, entUsagePath,
		map[string]any{"require_usage": true, "req_source": 1}, 0)
}
