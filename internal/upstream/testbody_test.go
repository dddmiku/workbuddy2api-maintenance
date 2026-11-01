// testbody_test.go 测试桩读取出站请求体的统一入口。
//
// 背景：2026-10-31 起 chat 出站请求体按官方客户端形态做 gzip 压缩
// （Content-Encoding: gzip）。测试桩若直接 io.ReadAll(r.Body)，拿到的是压缩
// 后的字节，所有断言正文的用例都会失配。真实上游也按 Content-Encoding 解压，
// 因此测试侧同样解压后再断言，才是对「上游看到的正文」的正确模拟。
package upstream

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

// gunzipForTest 测试用 gzip 解压（gzipBody 的逆操作）。
func gunzipForTest(t interface{ Fatalf(string, ...any) }, raw []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}
