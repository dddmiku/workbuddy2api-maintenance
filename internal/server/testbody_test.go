// testbody_test.go 测试桩读取出站请求体的统一入口（server 包）。
//
// 背景：2026-10-31 起 chat 出站请求体按官方客户端形态做 gzip 压缩
// （Content-Encoding: gzip，官方 6/6 抓包一致）。测试桩若直接读 r.Body，
// 拿到的是压缩字节（首字节 \x1f），JSON 断言必然失配。真实上游按
// Content-Encoding 解压，测试侧同样解压后再断言才是正确模拟。
package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
)

// readReqBody 读取出站请求体；Content-Encoding: gzip 时先解压。
// 读失败或解压失败返回已读到的原始字节（不 panic，便于用例给出可读断言）。
func readReqBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return raw
	}
	if r.Header.Get("Content-Encoding") != "gzip" {
		return raw
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return raw
	}
	return plain
}
