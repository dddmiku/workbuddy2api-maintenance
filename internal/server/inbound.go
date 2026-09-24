// ═══ 更新日志 ═══
// 2026-09-25：在鉴权后按有界流解码gzip/zstd请求，分别限制线上体积与解压后体积，不将编码头传给内部适配。
package server

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type decodedBody struct {
	io.Reader
	close func() error
}

func (r *decodedBody) Close() error { return r.close() }

func writeBodyReadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "request body exceeds the configured size or decoding window limit")
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
}

func (h *Handler) withDecodedRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		if encoding == "" || encoding == "identity" {
			next(w, r)
			return
		}
		if encoding != "gzip" && encoding != "zstd" {
			w.Header().Set("Accept-Encoding", "gzip, zstd")
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding", "request Content-Encoding must be gzip, zstd, or identity")
			return
		}
		wire := http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
		var reader io.ReadCloser
		var err error
		if encoding == "gzip" {
			reader, err = gzip.NewReader(wire)
		} else {
			memoryLimit := uint64(max(h.cfg.MaxBodyBytes, 8<<20))
			var decoder *zstd.Decoder
			decoder, err = zstd.NewReader(wire, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(memoryLimit), zstd.WithDecoderMaxWindow(memoryLimit))
			if err == nil {
				reader = decoder.IOReadCloser()
			}
		}
		if err != nil {
			_ = wire.Close()
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "compressed request exceeds the body size limit")
			} else {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_compressed_body", "invalid compressed request body")
			}
			return
		}
		body := &decodedBody{Reader: io.LimitReader(reader, h.cfg.MaxBodyBytes+1), close: func() error { return errors.Join(reader.Close(), wire.Close()) }}
		defer body.Close()
		request := r.Clone(r.Context())
		request.Header.Del("Content-Encoding")
		request.Header.Del("Content-Length")
		request.ContentLength = -1
		request.Body = body
		next(w, request)
	}
}
