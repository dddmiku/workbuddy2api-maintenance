// ═══ 更新日志 ═══
// 2026-10-17：初版——渠道拒绝（11128）时完整落盘出站报文，供事后精确定位。
package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// channelDumpDir 渠道拒绝样本的落盘目录（config upstream.channel_dump_dir 可覆盖）。
// 空 = 关闭（默认关闭：避免无谓写盘；排查时显式打开）。
var channelDumpDir string

// SetChannelDumpDir 设置渠道拒绝样本目录；空串关闭。启动时由配置注入。
func SetChannelDumpDir(dir string) { channelDumpDir = strings.TrimSpace(dir) }

// dumpChannelRejection 把一次渠道拒绝的**完整现场**写成 JSON 落盘。
//
// 落盘内容（三样，缺一不可——排查时要靠它们做逐字节对照）：
//  1. 出站请求头（含网关注入的每一个头）；
//  2. 出站请求体（**完整**，不截断；含所有改写后的字段与正文）；
//  3. 上游响应原文（**完整**，不截断；11128 信封里的 displayMsg/displayTips/requestId 都在这里）。
//
// 另外记下可对照的元信息：账号、realm、模型、上游 URL、时间。
//
// 为什么是落盘而不是打日志：日志按行截断（200 字节）会切掉响应正文，而 11128 的
// 有效信息（displayTips 指出「没通过渠道校验」）正好在正文靠后位置；请求体更是
// 几十万字节，只能整文件保存。文件名带时间戳与 uid 前缀，便于按时间线比对。
//
// 失败静默：落盘是诊断辅助，任何 IO 错误都不能影响请求处理本身。
func (c *Client) dumpChannelRejection(ctx context.Context, a *auth.Auth, body []byte,
	req *http.Request, status int, raw []byte) {

	dir := channelDumpDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	record := map[string]any{
		"at":          time.Now().Format(time.RFC3339Nano),
		"status":      status,
		"uid":         uidOf(a),
		"realm":       realmOf(a),
		"upstream":    urlOf(req),
		"headers":     headersOf(req),
		"request_len": len(body),
		"request":     string(body),
		"response":    string(raw),
	}
	encoded, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return
	}
	name := time.Now().Format("20060102-150405.000") + "-" + uid8Of(a) + ".json"
	// 原子写：先写临时文件再改名，避免读取方看到半截文件。
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return
	}
	_, writeErr := tmp.Write(encoded)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	_ = os.Rename(tmp.Name(), filepath.Join(dir, name))
	_ = ctx // 预留：将来若要在 ctx 里带请求级标签
}

// headersOf 取出站请求头为 map（同名字段按出现顺序拼接，便于与抓包逐项对照）。
func headersOf(req *http.Request) map[string]string {
	if req == nil {
		return nil
	}
	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = strings.Join(req.Header.Values(name), ", ")
	}
	return out
}

// uidOf 取账号 uid（nil 安全）。
func uidOf(a *auth.Auth) string {
	a = a.Snapshot()
	if a == nil {
		return ""
	}
	return a.UID
}

// realmOf 取账号 realm（nil 安全）。
func realmOf(a *auth.Auth) string {
	a = a.Snapshot()
	if a == nil {
		return ""
	}
	return a.Realm()
}

// uid8Of 取 uid 前 8 位（日志/文件名口径，与 logfmt.UID8 一致）。
func uid8Of(a *auth.Auth) string {
	uid := uidOf(a)
	if len(uid) >= 8 {
		return uid[:8]
	}
	if uid == "" {
		return "anon"
	}
	return uid
}

// urlOf 取请求 URL（nil 安全）。
func urlOf(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.String()
}

var _ = sync.Mutex{} // 预留：将来若需要串行化落盘
