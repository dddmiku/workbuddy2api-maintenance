// ═══ 更新日志 ═══
// 2026-10-29：新增。Trae 账号的手机号只能从 GetUserInfo 取，用于面板区分账号。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

// GetUserInfoPath 取账号资料（含掩码手机号）。
//
// 2026-10-29 从网页 JS（static/js/65960.*.js）挖出：
//
//	N=n=>({method:"POST",url:L+"/GetUserInfo",headers:n})   // L = /cloudide/api/v3/trae
//
// 与签到/积分不同，这个接口**认 Cloud-IDE-JWT**（Authorization 头），不需要
// passport 的 Web 会话 cookie——网页端登录后拿到的也是同一枚 Cloud-IDE token
// （存在 localStorage 的 "Cloud-IDE-Token"，由 /cloudide/api/v3/common/GetUserToken 换取）。
//
// 实测：用账号文件里的 accessToken 直接调用即返回 200，6 个账号全部成功。
const GetUserInfoPath = "/cloudide/api/v3/trae/GetUserInfo"

// UserInfo 是面板需要的账号资料子集。
type UserInfo struct {
	UserID      string // 上游 UserID（与账号文件里的 uid 一致）
	ScreenName  string // 昵称，形如「用户6126656924」
	Mobile      string // 掩码手机号，形如 138******00（上游字段 NonPlainTextMobile）
	Email       string // 掩码邮箱，可能为空
	RegisterTime string
	LastLoginType string
}

// GetUserInfo 取账号资料。ctx 为 nil 时用 Background。
//
// 上游把手机号放在 NonPlainTextMobile（已掩码，形如 138******00），
// 明文手机号不会下发——这是 passport 侧的隐私策略，不是权限不足。
func GetUserInfo(ctx context.Context, a *auth.Auth) (UserInfo, error) {
	if a == nil {
		return UserInfo{}, fmt.Errorf("trae user info: nil auth")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		TraeAPIHost+GetUserInfoPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return UserInfo{}, err
	}
	// 头族与 chat 一致（Cloud-IDE-JWT + x-uid），签到那套（checkinHeaders）
	// 带的是签到专用设备头，实测对这个接口同样可用，但没必要引入额外变量。
	for k, vs := range buildHeaders(a) {
		// buildHeaders 会带 Accept: text/event-stream，这个接口返回 JSON。
		if k == "Accept" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Transport: chatTransport()}).Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("trae user info: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return UserInfo{}, fmt.Errorf("trae user info read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, fmt.Errorf("trae user info: HTTP %d: %s",
			resp.StatusCode, truncate(string(data), 300))
	}
	var payload struct {
		Result struct {
			UserID              string `json:"UserID"`
			ScreenName          string `json:"ScreenName"`
			NonPlainTextMobile  string `json:"NonPlainTextMobile"`
			NonPlainTextEmail   string `json:"NonPlainTextEmail"`
			RegisterTime        string `json:"RegisterTime"`
			LastLoginType       string `json:"LastLoginType"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return UserInfo{}, fmt.Errorf("trae user info parse: %w", err)
	}
	return UserInfo{
		UserID:        payload.Result.UserID,
		ScreenName:    payload.Result.ScreenName,
		Mobile:        payload.Result.NonPlainTextMobile,
		Email:         payload.Result.NonPlainTextEmail,
		RegisterTime:  payload.Result.RegisterTime,
		LastLoginType: payload.Result.LastLoginType,
	}, nil
}
