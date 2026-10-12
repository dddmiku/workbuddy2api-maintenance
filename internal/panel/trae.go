// ═══ 更新日志 ═══
// 2026-10-13：初版——Trae CN 网页授权登录的 Go 版（与 panel/trae_login.py 同口径）。
//
// 为什么不做本机助手
// ------------------
// Trae 授权页强制把回调地址设成 http://127.0.0.1:<port>/authorize，浏览器网页出于
// 沙箱限制无法监听本机端口，所以旧实现要求用户下载并运行一个本地监听器来接回调，
// 多一个安装步骤、也多一整类「助手没启动」的故障。这里换成**用户手动回传**：授权
// 完成后浏览器停在一个打不开的 127.0.0.1 地址上，但**地址栏里的查询串带着全部
// 凭据**（userJwt/userInfo，或老流程的 refreshToken）。用户把整条 URL 复制回来粘贴
// 即可，凭据由服务器自己解析。
//
// 四种可接受的输入形态
// --------------------
// 用户复制到的东西形态不定，所以解析必须覆盖：
//  1. 完整回调 URL：http://127.0.0.1:8765/authorize?userJwt=...
//  2. 只有查询串：userJwt=...&userInfo=...
//  3. 裸 refreshToken（含 base64 的 + / = 与 `.` 尾巴）
//  4. 授权页链接（.../authorization?...）——走错一步，明确报错而不是去换 token
//
// 判定顺序很关键：**裸 token 的形状检查必须放在最后**。真实 refreshToken 里含
// `=`（base64 填充），若先按查询串解析会得到一个 `...-E` = `=.18dd...` 的假参数，
// 于是「看起来像查询串」却不是凭据。所以先按「解析结果里有没有已知凭据字段」判定，
// 再回退到裸 token 形状检查。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Trae 授权相关常量（与 panel/trae_login.py 逐字一致）。
const (
	TraeClientID     = "ono9krqynydwx5"
	TraeAuthURL      = "https://www.trae.cn/authorization"
	TraeExchangeHost = "https://api.trae.cn"
	TraeExchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"

	// TraeCallbackPort / TraeCallbackURL 是授权页要求的本机回调地址：端口只是为了让
	// URL 合法，我们不监听它。URL 写成字面量而不是 fmt.Sprintf，是为了让它能当常量用。
	TraeCallbackPort = 8765
	TraeCallbackURL  = "http://127.0.0.1:8765/authorize"
)

// Trae 解析链的用户可见报错。文案会直接显示在面板上，逐字与 Python 版一致。
var (
	ErrTraeEmptyInput   = errors.New("没有收到内容，请把浏览器地址栏里的完整链接粘贴进来")
	ErrTraeAuthPageLink = errors.New("这是 Trae 授权页的链接，不是授权完成后的回调链接。请先在 Trae 页面完成登录，然后复制跳转到的 127.0.0.1 那条地址。")
	ErrTraeUnparseable  = errors.New("无法从粘贴内容里解析出授权参数，请确认复制的是完整链接")
	ErrTraeNoUserJWT    = errors.New("粘贴的内容里没有有效的 userJwt，请确认授权已完成")
)

var (
	// traeBareTokenRe 是裸 token 的宽松形状检查：base64 字符集 + `.` `-` `_` `~`。
	// 只用来把「明显是别的东西」（中文说明、含空格的句子、太短的串）挡掉，
	// 精确校验是上游的事。
	traeBareTokenRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=\-]+$`)

	// traeCredentialParams 是回调里可能携带凭据的字段名。只有出现其中之一，这段文本
	// 才算「回调参数」而不是「一个恰好含 = 的裸 token」。
	traeCredentialParams = map[string]struct{}{
		"refreshToken": {}, "refresh_token": {}, "data": {},
		"userJwt": {}, "user_jwt": {}, "code": {}, "authCodeInfo": {}, "auth_code_info": {},
	}

	// traeExchangeClient 是包级客户端，测试可替换成 httptest.Server 或桩 RoundTripper。
	traeExchangeClient = &http.Client{Timeout: 20 * time.Second}
)

// TraeBuildAuthURL 拼出 Trae 网页授权 URL（参数与 Trae 官网一致）。
//
// callback_url 为空时用默认的本机地址（需要用户手工回传）；传入公网回调地址时，
// Trae 授权完成后会直接跳回服务器，凭据自动落盘。
//
// 用 url.Values.Encode() 输出：键按字典序稳定排序、值按查询串规则转义（`:`→%3A、
// `/`→%2F、空格→`+`），与 Python 的 urllib.parse.urlencode 编码结果一致。
func TraeBuildAuthURL(loginTraceID, machineID, deviceID, callbackURL string) string {
	if callbackURL == "" {
		callbackURL = TraeCallbackURL
	}
	params := url.Values{
		"login_version":     {"1"},
		"auth_from":         {"solo"},
		"login_channel":     {"native_ide"},
		"plugin_version":    {"2.3.24254"},
		"auth_type":         {"local"},
		"client_id":         {TraeClientID},
		"redirect":          {"0"},
		"login_trace_id":    {loginTraceID},
		"auth_callback_url": {callbackURL},
		"machine_id":        {machineID},
		"device_id":         {deviceID},
		"x_device_id":       {deviceID},
		"x_machine_id":      {machineID},
		"x_device_brand":    {"Mac14,7"},
		"x_device_type":     {"mac"},
		"x_os_version":      {"macOS 26.4.1"},
		"x_env":             {""},
		"x_app_version":     {"0.1.7"},
		"x_app_type":        {"stable"},
		"hide_saas_login":   {"true"},
	}
	return TraeAuthURL + "?" + params.Encode()
}

// TraeParseCallback 从用户粘贴的回调 URL（或裸查询串 / 裸 token）里解析出 Trae 凭据。
//
// 解析顺序（**不能调换**）：
//  1. 文本含 `?` 或 `://` 时先按 URL 解析，取 query；
//  2. query 里没有已知凭据字段时，把整段文本当成裸查询串（补一个假 scheme）；
//  3. 仍没有凭据字段时，才按裸 refreshToken 形状判断。
//
// 第 3 步必须在最后：裸 token 里含 `=`，第 2 步会把它解析成一个假参数，只有先确认
// 「没有已知凭据字段」才会走到裸 token 分支。
func TraeParseCallback(raw string) (map[string]any, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, ErrTraeEmptyInput
	}

	// 走错一步的常见情形：把「授权页」链接贴回来了，而不是授权完成后的回调。
	// 直接说清楚，比让它去换 token 然后报一个「token 无效」要好得多。
	if strings.Contains(text, "trae.cn/authorization") || strings.Contains(text, "/authorization?") {
		return nil, ErrTraeAuthPageLink
	}

	query := map[string]string{}
	if strings.Contains(text, "?") || strings.Contains(text, "://") {
		query = traeParseQuery(traeRawQuery(text))
	}
	if !traeHasCredentialParam(query) {
		// 裸查询串形态：补一个假的 scheme 让 urlsplit 能解析出 query。
		query = traeParseQuery(traeRawQuery("http://x/?" + strings.TrimLeft(text, "?")))
	}
	if !traeHasCredentialParam(query) {
		if traeLooksLikeBareToken(text) {
			query = map[string]string{"refreshToken": text}
		} else {
			return nil, ErrTraeUnparseable
		}
	}

	return traeCredentialsFromQuery(query)
}

// TraeExchangeRefreshToken 用 refreshToken 兑换 Cloud-IDE-JWT（Trae 官网同款接口）。
//
// 老授权流程的回调只带 refreshToken，需要这一步换取可用的 JWT。
func TraeExchangeRefreshToken(ctx context.Context, refreshToken, clientID, host string) (map[string]any, error) {
	if refreshToken == "" {
		return map[string]any{}, nil
	}
	base := host
	if base == "" {
		base = TraeExchangeHost
	}
	base = strings.TrimRight(base, "/")
	endpoint := base + TraeExchangePath

	payload, err := json.Marshal(map[string]any{
		"ClientID":     traeClientIDOrDefault(clientID),
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	if err != nil {
		return nil, fmt.Errorf("ExchangeToken 请求失败: %s", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ExchangeToken 请求失败: %s", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := traeExchangeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ExchangeToken 请求失败: %s", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ExchangeToken 请求失败: %s", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ExchangeToken HTTP %d: %s", resp.StatusCode, traeTruncate(string(raw), 500))
	}
	body := string(raw)

	result := traeJSONLoadsSafe(body)
	var data any = result
	if value, ok := result["Result"]; ok && traePyTruthy(value) {
		data = value
	} else if value, ok := result["result"]; ok && traePyTruthy(value) {
		data = value
	}
	dataMap, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ExchangeToken 返回结构异常: %s", traeTruncate(body, 300))
	}
	token := traeFirstString(dataMap, "Token", "token", "AccessToken", "accessToken")
	if token == "" {
		return nil, fmt.Errorf("ExchangeToken 未返回 Token（refreshToken 可能已失效）: %s", traeTruncate(body, 300))
	}
	// UserInfo 本身是一个 **JSON 字符串**，要再解析一次。
	userInfo := traeJSONLoadsSafe(traePyStr(traeFirstValue(dataMap, "UserInfo", "userInfo")))
	refresh := traeFirstString(dataMap, "RefreshToken", "refreshToken")
	if refresh == "" {
		refresh = refreshToken
	}
	return map[string]any{
		"token":         token,
		"refreshToken":  refresh,
		"tokenExpireAt": traePyStr(traeFirstValue(dataMap, "TokenExpireAt", "tokenExpireAt")),
		"userInfo":      userInfo,
		"clientId":      traeFirstNonEmpty(traeFirstString(dataMap, "ClientID", "clientId"), clientID),
		"host":          base,
	}, nil
}

// TraeNormalizeExpire 把 TokenExpireAt（秒/毫秒时间戳字符串）归一成 Unix 秒；无法解析返回 0。
//
// 与 Python 的差异：`float("nan")` / `float("inf")` 在 Python 里能解析成功但随后的
// `int()` 会抛异常（NaN）或 OverflowError（Inf），属于未定义行为；这里统一返回 0。
func TraeNormalizeExpire(raw any) int64 {
	text := strings.TrimSpace(traePyStr(raw))
	if text == "" {
		return 0
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value > 1e12 { // 毫秒
		value /= 1000.0
	}
	return int64(value)
}

// traeCredentialsFromQuery 按 Python 版的完整回退链把查询串解析成面板用的凭据字典。
//
// 返回的键与 Python 的 dict 完全一致：access_token / refresh_token / token_expire_at
// （永远是字符串，未知时为空串）/ uid / nickname / region / host。
func traeCredentialsFromQuery(query map[string]string) (map[string]any, error) {
	userJWT := traeJSONLoadsSafe(traeQueryValue(query, "userJwt", "user_jwt"))
	token := traeFirstString(userJWT, "Token", "token")
	refresh := traeFirstString(userJWT, "RefreshToken", "refreshToken")
	if refresh == "" {
		refresh = traeQueryValue(query, "refreshToken", "refresh_token", "data")
	}
	tokenExpire := traeFirstValue(userJWT, "TokenExpireAt", "tokenExpireAt")
	userInfo := traeJSONLoadsSafe(traeQueryValue(query, "userInfo", "user_info"))
	userID := traeFirstString(userInfo, "UserID", "userId", "userID")
	if userID == "" {
		userID = traeQueryValue(query, "userId")
	}
	region := traeFirstString(userInfo, "Region", "region")
	if region == "" {
		region = traeQueryValue(query, "region")
	}
	if region == "" {
		region = "CN"
	}
	clientID := traeFirstString(userJWT, "ClientID", "clientId")
	if clientID == "" {
		clientID = traeQueryValue(query, "clientID", "clientId", "client_id")
	}
	host := traeQueryValue(query, "host")
	if host == "" {
		host = traeFirstString(userInfo, "Host", "host")
	}

	// 老流程：回调只带 refreshToken，必须兑换成 Cloud-IDE-JWT 才能用。
	if token == "" && refresh != "" {
		exchanged, err := TraeExchangeRefreshToken(context.Background(), refresh, traeClientIDOrDefault(clientID), host)
		if err != nil {
			return nil, err
		}
		if value := traePyStr(exchanged["token"]); value != "" {
			token = value
		}
		if value := traePyStr(exchanged["refreshToken"]); value != "" {
			refresh = value
		}
		if value, ok := exchanged["tokenExpireAt"]; ok && traePyTruthy(value) {
			tokenExpire = value
		}
		exchangedInfo, _ := exchanged["userInfo"].(map[string]any)
		if len(exchangedInfo) > 0 {
			userInfo = exchangedInfo
		}
		if userID == "" {
			userID = traeFirstString(exchangedInfo, "UserID")
		}
		if value := traePyStr(exchanged["host"]); value != "" {
			host = value
		}
	}

	if token == "" {
		return nil, ErrTraeNoUserJWT
	}

	tokenExpireText := ""
	if traePyTruthy(tokenExpire) {
		tokenExpireText = traePyStr(tokenExpire)
	}
	return map[string]any{
		"access_token":    token,
		"refresh_token":   refresh,
		"token_expire_at": tokenExpireText,
		"uid":             userID,
		"nickname":        traeFirstString(userInfo, "ScreenName", "screenName"),
		"region":          region,
		"host":            traeFirstNonEmpty(host, TraeExchangeHost),
	}, nil
}

// traeRawQuery 对应 Python 的 urlsplit(...).query：文本不含 `://` 时补一个假 scheme。
func traeRawQuery(text string) string {
	target := text
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return parsed.RawQuery
}

// traeParseQuery 对应 Python 的 dict(parse_qsl(...))。
//
// 两个与 Python 对齐的细节：同名键保留**最后一个**（dict 覆盖语义）；空值键被丢弃
// （parse_qsl 默认 keep_blank_values=False）——否则 `?refreshToken=` 会被误判成
// 「有凭据字段」。
func traeParseQuery(rawQuery string) map[string]string {
	if rawQuery == "" {
		return map[string]string{}
	}
	values, _ := url.ParseQuery(rawQuery) // 与 parse_qsl 一样宽松：出错时用已解析部分
	out := make(map[string]string, len(values))
	for key, list := range values {
		if len(list) == 0 {
			continue
		}
		value := list[len(list)-1]
		if value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func traeHasCredentialParam(query map[string]string) bool {
	for key := range query {
		if _, ok := traeCredentialParams[key]; ok {
			return true
		}
	}
	return false
}

// traeLooksLikeBareToken 判断一段文本是否像一个裸 token（宽松形状检查，见 traeBareTokenRe）。
func traeLooksLikeBareToken(text string) bool {
	length := utf8.RuneCountInString(text)
	if length < 16 || length > 4096 {
		return false
	}
	return traeBareTokenRe.MatchString(text)
}

// traeQueryValue 取第一个非空的查询值（对应 Python 的 `q.get(a) or q.get(b)` 链）。
func traeQueryValue(query map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := query[key]; value != "" {
			return value
		}
	}
	return ""
}

// traeFirstValue 取第一个真值字段（对应 Python 的 `doc.get(a) or doc.get(b)`）。
func traeFirstValue(doc map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := doc[key]; ok && traePyTruthy(value) {
			return value
		}
	}
	return nil
}

// traeFirstString 取第一个真值字段并转成字符串。
func traeFirstString(doc map[string]any, keys ...string) string {
	return traePyStr(traeFirstValue(doc, keys...))
}

// traeClientIDOrDefault 对应 Python 的 `client_id or TRAE_CLIENT_ID`。
func traeClientIDOrDefault(clientID string) string {
	if clientID == "" {
		return TraeClientID
	}
	return clientID
}

func traeFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// traeJSONLoadsSafe 对应 Python 的 _json_loads_safe：空串 / 非法 JSON / 非对象一律返回 {}。
//
// 用 UseNumber 保留数字原样，避免 `"TokenExpireAt": 1791700000` 被解成 float64 后在
// 字符串化时变形成 `1.7917e+09`（Python 侧 int 的 str() 是 `1791700000`）。
func traeJSONLoadsSafe(value any) map[string]any {
	text, ok := value.(string)
	if !ok || text == "" {
		return map[string]any{}
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var doc map[string]any
	if err := decoder.Decode(&doc); err != nil {
		return map[string]any{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF { // json.loads 不容忍尾随内容
		return map[string]any{}
	}
	if doc == nil {
		return map[string]any{}
	}
	return doc
}

// traePyTruthy 复刻 Python 的布尔真值语义（""、0、None、空容器为假）。
func traePyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		number, err := v.Float64()
		if err != nil {
			return true // 解析不了的数字串按「非空」处理
		}
		return number != 0
	case float64:
		return v != 0
	case float32:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case int32:
		return v != 0
	case uint64:
		return v != 0
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	}
	return true
}

// traePyStr 复刻 Python 的 str()：None→""（配合 `raw or ""`）、True/False、数字原样。
func traePyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	}
	return fmt.Sprint(value)
}

// traeTruncate 按字符（而非字节）截断，对应 Python 的 body[:n]——避免把中文切坏。
func traeTruncate(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}
