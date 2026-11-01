// ═══ 更新日志 ═══
// 2026-10-17：初版——按 WB2API_DUMP_HEADERS 环境变量把真实出站头打进日志。
package upstream

import (
	"bytes"
	"compress/gzip"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

// dumpOutboundHeaders 在设置 WB2API_DUMP_HEADERS=1 时把本次 chat 出站请求的
// 全部头写进日志（脱敏 Authorization / X-Device-Token）。
//
// 为什么需要它：判断「网关出站头组是否与官方客户端同形」只能靠真实抓取。
// 2026-10-17 排查 global 全部被 11128 拒绝时，仅凭代码阅读无法确定实际发出的
// 头（多层 injector 叠加、部分头按配置省略），而生产日志此前只记录响应体。
// 该开关默认关闭，只在排障时临时打开：开启后会逐请求刷日志，正常生产不要常开。
//
// 用法：容器里 export WB2API_DUMP_HEADERS=1 后重启，或 docker run -e。
func dumpOutboundHeaders(headers map[string][]string, uid string) {
	if strings.TrimSpace(os.Getenv("WB2API_DUMP_HEADERS")) != "1" {
		return
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("[dump] chat 出站头 uid=")
	b.WriteString(logfmtUID(uid))
	// 设备 token 是否存在 + 客户端持有的全局值长度：定位「token 没发出去」时区分
	// 「配置没读到」与「注入逻辑没走到」。
	if _, ok := headers["X-Device-Token"]; ok {
		b.WriteString(" [device-token=present]")
	} else {
		b.WriteString(" [device-token=MISSING]")
	}
	for _, k := range keys {
		v := strings.Join(headers[k], ",")
		switch strings.ToLower(k) {
		case "authorization":
			v = "Bearer <redacted>"
		case "x-device-token":
			v = "<redacted len=" + strconv.Itoa(len(v)) + ">"
		}
		b.WriteString(" | ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(v)
	}
	log.Print(b.String())
}

// logfmtUID 只保留 UID 前 8 位（与包内其他日志同口径）。
func logfmtUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// gzipBody 把请求体压成 gzip（官方桌面端 chat 请求体恒为 gzip）。
//
// 为什么需要它（2026-10-31 双版本抓包实证）：官方 chat 请求 6/6 携带
// `Content-Encoding: gzip`，195KB 正文压成 73KB；网关此前发明文 JSON。
// 一个声称 WorkBuddy/5.9.3 的客户端却从不压缩请求体，是与官方最直观的
// 形态差异之一。
//
// 失败时返回错误，调用方保持明文发送（不因压缩失败打断业务）。
func gzipBody(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
