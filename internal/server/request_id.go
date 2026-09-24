// ═══ 更新日志 ═══
// 2026-09-25：为入口、上游和请求日志生成同一个不可伪造的请求标识，便于定位跨客户端故障。
package server

import (
	"context"
	"crypto/rand"
	"net/http"
)

type requestIDKey struct{}

func identifyRequest(w http.ResponseWriter, r *http.Request) *http.Request {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	if id == "" {
		id = "req_" + rand.Text()
	}
	w.Header().Set("X-Request-ID", id)
	return r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
}

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}
