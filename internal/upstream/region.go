// region.go 国际版账号「注册地/区域」补全。
//
// 背景（2026-09-20 实测）：global 账号若在官方登录流程里没有确认国家/地区，
// chat 会被上游以 429 + code 14017「The trial version is not yet activated.
// Please log out of your current account and log in again to activate it
// immediately and start your free trial.」拒绝，且**永不**自愈——网关只能把它当
// 账号级软冷却轮换掉，整池可用号会随着这类账号增多而缩水。官方网页登录在
// 「Complete Your Profile」这一步提交 countryCode；对已经在用的号补交同样属性
// 即可立刻恢复（实测：补交后同号 chat 从 429 直接变 200）。
//
// 两个端点（都走 global base，即 www.workbuddy.ai）：
//   - POST /billing/area/get-user-area-info  {"action":"getUserAreaInfo"}
//     → data 是 JSON 字符串，内层 data 带按出口 IP 探测到的国家/地区
//     （IOS2/IOS3/enName/name/code）。官方页面就是用它做「当前地区」默认值。
//   - POST /console/login/account  {"attributes":{countryCode:[...],
//     countryFullName:[...],countryName:[...]}} → 写入账号属性。
//   - GET /auth/realms/copilot/overseas/user/register?userId=<uid>
//     → 已登记区域返回 {"code":200,"msg":"register success"}；未登记返回
//     {"code":500,"msg":"register failed:register region required"}。实测它同时是
//     trial 的激活入口（返回 success 的号 chat 立即可用），因此修复序列先调它，
//     未通过再补区域并重调。
//
// 注意：get-user-area-info 返回的是出口 IP 的地理位置，**不是**账号已登记的地区，
// 所以无法用它判断「是否已登记」；补交动作本身幂等，调用方只在 14017 命中时触发一次。
//
// ═══ 更新日志 ═══
// 2026-09-20：新增。修复 global 账号缺注册地导致的 14017 永久不可用。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

// 区域相关端点（global base）。
const (
	userAreaInfoPath = "/billing/area/get-user-area-info"
	loginAccountPath = "/console/login/account"
	// registerPath 国际版 register/激活端点（GET，仅需 query userId）。
	registerPath = "/auth/realms/copilot/overseas/user/register"
)

// regionRequiredMarker 上游未登记区域的判定文案（大小写不敏感匹配）。
const regionRequiredMarker = "register region required"

// UserArea 上游按出口 IP 探测到的国家/地区。
type UserArea struct {
	IOS2   string `json:"IOS2"`
	IOS3   string `json:"IOS3"`
	EnName string `json:"enName"`
	Name   string `json:"name"`
	Code   string `json:"code"`
}

// Complete 报告该地区信息是否足以补交账号属性。
func (u UserArea) Complete() bool {
	return u.IOS2 != "" && u.EnName != "" && u.Code != ""
}

// regionInner get-user-area-info 的内层信封。注意 doJSON 已经剥掉最外层信封并返回
// data 字段：正常形态下它是一个 JSON **字符串**，再解一次才是本结构；
// 少数情况下上游直接给对象，两种都兼容（见 UserArea）。
type regionInner struct {
	Code int      `json:"code"`
	Msg  string   `json:"msg"`
	Data UserArea `json:"data"`
}

// consoleHeaders 构造 console/billing 属性类端点的出站头。
// 与 chat 同源（CommonHeaders 提供 Origin/Referer/UA/设备头），但不声明流式 Accept，
// 也不带任何对话头族——这是账号属性接口，不是模型请求。
func (c *Client) consoleHeaders(req *http.Request, a *auth.Auth) {
	a = a.Snapshot()
	c.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json")
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a != nil && a.IsGlobal() {
		c.injectGlobalChatHeaders(req, a)
	} else if a != nil {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
	}
	c.injectDeviceToken(req, a)
}

// globalBaseFor 返回账号所在 realm 的 global 属性域 base；非 global 账号返回空串
// （区域补全是国际版专有问题，CN 无此端点，调用方据此短路）。
func (c *Client) globalBaseFor(a *auth.Auth) string {
	if a == nil || !c.globalOn(a) {
		return ""
	}
	return c.globalChatBase()
}

// consoleJSON 向 base+path 发一次 JSON 请求并解外层信封（data 原样返回）。
func (c *Client) consoleJSON(a *auth.Auth, base, path string, body any) (json.RawMessage, error) {
	a = a.Snapshot()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, rdr)
	if err != nil {
		return nil, err
	}
	c.consoleHeaders(req, a)
	return c.doJSON(req)
}

// UserArea 读取上游按出口 IP 探测到的国家/地区（官方「当前地区」默认值来源）。
func (c *Client) UserArea(a *auth.Auth) (UserArea, error) {
	base := c.globalBaseFor(a)
	if base == "" {
		return UserArea{}, fmt.Errorf("user area: only global accounts")
	}
	raw, err := c.consoleJSON(a, base, userAreaInfoPath, map[string]string{"action": "getUserAreaInfo"})
	if err != nil {
		return UserArea{}, err
	}
	// consoleJSON 走 doJSON：外层 code 非零已转成错误；这里拿到的是 data 字段原文。
	var inner regionInner
	if len(raw) > 0 {
		if raw[0] == '"' {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return UserArea{}, fmt.Errorf("user area data: %w", err)
			}
			if err := json.Unmarshal([]byte(text), &inner); err != nil {
				return UserArea{}, fmt.Errorf("user area inner: %w", err)
			}
		} else if err := json.Unmarshal(raw, &inner); err != nil {
			return UserArea{}, fmt.Errorf("user area object: %w", err)
		}
	}
	if !inner.Data.Complete() {
		return UserArea{}, fmt.Errorf("user area incomplete: %+v", inner.Data)
	}
	return inner.Data, nil
}

// regionRawJSON 发一次请求并返回原始 body（不做信封解包）。
//
// 为什么不能复用 doJSON：register 端点成功时返回 {"code":200,...}，而 doJSON 把
// 任何非零 code 当失败——200 会被误判成错误。这里按原文判定。
func (c *Client) regionRawJSON(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return raw, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// completeRegionOnce 向 register 端点发一次激活请求；返回上游是否报「缺区域」。
func (c *Client) completeRegionOnce(a *auth.Auth, base string) (needRegion bool, err error) {
	a = a.Snapshot()
	url := base + registerPath + "?userId=" + a.UID
	req, rerr := http.NewRequest(http.MethodGet, url, nil)
	if rerr != nil {
		return false, rerr
	}
	c.consoleHeaders(req, a)
	raw, err := c.regionRawJSON(req)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) && containsAnyFold(ue.Msg, regionRequiredMarker) {
			return true, nil
		}
		return false, err
	}
	// 缺区域：HTTP 200 + {"code":500,"msg":"register failed:register region required"}。
	return containsAnyFold(string(raw), regionRequiredMarker), nil
}

// CompleteRegion 修复 global 账号的 14017（trial not activated）。两步幂等：
//  1. 调 register 端点激活 trial——已激活号返回 success，无副作用；
//  2. 若上游报「register region required」（账号没在官方登录流程里确认国家/地区），
//     按出口 IP 探测到的地区补交账号属性，再调一次 register。
//
// 只对 global 账号生效；返回补交的地区供日志记录（无需补区域时返回零值）。
func (c *Client) CompleteRegion(a *auth.Auth) (UserArea, error) {
	base := c.globalBaseFor(a)
	if base == "" {
		return UserArea{}, fmt.Errorf("complete region: only global accounts")
	}
	needRegion, err := c.completeRegionOnce(a, base)
	if err == nil && !needRegion {
		return UserArea{}, nil // register 直接成功：trial 已激活，无需改账号属性
	}
	area, err := c.UserArea(a)
	if err != nil {
		if needRegion {
			return UserArea{}, err
		}
		// register 本身报错、地区又探测不到：没有可补的属性，按原错误上抛。
		return UserArea{}, fmt.Errorf("register: %w", err)
	}
	body := map[string]any{
		"attributes": map[string][]string{
			"countryCode":     {area.Code},
			"countryFullName": {area.EnName},
			"countryName":     {area.IOS2},
		},
	}
	if _, err := c.consoleJSON(a, base, loginAccountPath, body); err != nil {
		return UserArea{}, err
	}
	// 补区域后再激活一次：官方页面提交成功后也会走同一跳转/激活链。
	if _, rerr := c.completeRegionOnce(a, base); rerr != nil {
		return area, rerr
	}
	return area, nil
}

// RegionRequired 判定上游 chat 错误是否是「缺注册地」形态的 14017。
//
// 上游 chat 侧的 14017 文案只讲 trial 未激活，并不直说 region；但同一账号调
// /auth/realms/copilot/overseas/user/register 时，缺注册地的号返回
// "register failed:register region required"，已登记号返回 "register success"。
// 因此这里以 14017 为入口信号，由调用方在命中后做一次幂等补交（见文件头注释）。
func RegionRequired(status int, body string) bool {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden && status != http.StatusBadRequest {
		return false
	}
	if !containsAnyFold(body, `"code":14017`, "code=14017") {
		return false
	}
	return containsAnyFold(body, "trial")
}

// containsAnyFold 大小写不敏感的任一子串命中。
func containsAnyFold(s string, needles ...string) bool {
	lower := strings.ToLower(s)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}
