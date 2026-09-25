// ═══ 更新日志 ═══
// 2026-09-25：请求消费明细只向内部管理通道开放，查询参数严格校验，保留范围与累计账本分开。
package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"workbuddy2api/internal/requestlog"
)

func (h *Handler) requestHistory(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Requests == nil {
		_ = writeJSON(w, 503, map[string]any{"ok": false, "message": "请求明细尚未启用"})
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		_ = writeJSON(w, 400, map[string]any{"ok": false, "message": "筛选参数格式错误"})
		return
	}
	allowed := map[string]bool{"key_id": true, "model": true, "status": true, "request_id": true, "offset": true, "limit": true}
	for key, items := range values {
		if !allowed[key] || len(items) != 1 {
			_ = writeJSON(w, 400, map[string]any{"ok": false, "message": "包含重复或不支持的筛选参数"})
			return
		}
	}
	query := requestlog.Query{KeyID: values.Get("key_id"), Model: values.Get("model"), Status: requestlog.Status(values.Get("status")), RequestID: values.Get("request_id")}
	for name, target := range map[string]*int{"offset": &query.Offset, "limit": &query.Limit} {
		if values.Has(name) {
			n, e := strconv.Atoi(values.Get(name))
			if e != nil || n < 0 || (name == "limit" && n == 0) {
				_ = writeJSON(w, 400, map[string]any{"ok": false, "message": "分页参数无效"})
				return
			}
			*target = n
		}
	}
	page, err := h.cfg.Requests.List(query)
	if err != nil {
		h.requestHistoryError(w, err)
		return
	}
	_ = writeJSON(w, 200, map[string]any{"ok": true, "items": page.Items, "total": page.Total, "offset": page.Offset, "limit": page.Limit, "recovery": page.Recovery, "recording_errors": h.requestLogErrors.Load(), "retention": map[string]any{"max_records": requestlog.DefaultMaxRecords, "max_age_seconds": int64(requestlog.DefaultMaxAge.Seconds()), "max_bytes": requestlog.DefaultMaxBytes}})
}

func (h *Handler) requestDetail(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Requests == nil {
		_ = writeJSON(w, 503, map[string]any{"ok": false, "message": "请求明细尚未启用"})
		return
	}
	if r.URL.RawQuery != "" {
		_ = writeJSON(w, 400, map[string]any{"ok": false, "message": "详情接口不接受额外参数"})
		return
	}
	record, err := h.cfg.Requests.Get(r.PathValue("requestID"))
	if err != nil {
		h.requestHistoryError(w, err)
		return
	}
	_ = writeJSON(w, 200, map[string]any{"ok": true, "record": record})
}

func (h *Handler) requestHistoryError(w http.ResponseWriter, err error) {
	status, message := 503, "请求明细暂不可读，请查看用量统计与服务日志"
	if errors.Is(err, requestlog.ErrNotFound) {
		status, message = 404, "该请求已超出留存范围，或尚未完成记录"
	}
	if errors.Is(err, requestlog.ErrInvalidQuery) {
		status, message = 400, "筛选条件无效"
	}
	_ = writeJSON(w, status, map[string]any{"ok": false, "message": message})
}
