// ═══ 更新日志 ═══
// 2026-09-25：拒绝格式损坏的查询参数，避免标准库部分解析把调用者的运输选项静默删掉。
// 2026-09-25：Gemini入口复用现有密钥权限和有界解压，规范错误结构并从内部请求移除查询凭据。
package server

import (
	"fmt"
	"net/http"
	"net/url"
)

// Model discovery shares /v1/models with OpenAI. The Google credential form
// selects its response shape; /v1beta/models is always unambiguous.
func geminiDiscoveryHint(r *http.Request) bool {
	return len(r.Header.Values("X-Goog-Api-Key")) != 0 || r.URL.Query().Has("key")
}

func geminiAuthRequest(r *http.Request) (*http.Request, error) {
	request := r.Clone(r.Context())
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		// Do not echo the parse error: it can contain a query API key.
		request.URL.RawQuery = ""
		request.RequestURI = request.URL.RequestURI()
		return request, fmt.Errorf("invalid URL query parameters")
	}
	keys := query["key"]
	query.Del("key")
	request.URL.RawQuery = query.Encode()
	request.RequestURI = request.URL.RequestURI()
	if len(keys) > 1 {
		return request, fmt.Errorf("multiple API key query parameters are not supported")
	}
	if values := request.Header.Values("Authorization"); len(values) > 0 {
		if len(values) != 1 {
			return request, fmt.Errorf("multiple Authorization headers are not supported")
		}
		// An explicit header is authoritative, even if invalid or empty.
		return request, nil
	}
	if values := request.Header.Values("X-Goog-Api-Key"); len(values) > 0 {
		if len(values) != 1 {
			return request, fmt.Errorf("multiple x-goog-api-key headers are not supported")
		}
		request.Header.Set("Authorization", "Bearer "+values[0])
		return request, nil
	}
	if len(keys) == 1 {
		request.Header.Set("Authorization", "Bearer "+keys[0])
	}
	return request, nil
}

func (h *Handler) withGeminiProtocol(next http.HandlerFunc, decodeBody bool) http.HandlerFunc {
	if decodeBody {
		next = h.withDecodedRequest(next)
	}
	authenticated := h.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		writer := newGeminiWriter(w)
		defer writer.finish()
		request, err := geminiAuthRequest(r)
		if err != nil {
			writer.failure(http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		authenticated(writer, request)
	}
}

func (h *Handler) modelProtocolDiscovery(openAI http.HandlerFunc) http.HandlerFunc {
	standard := h.withDiscoveryAuth(openAI)
	google := h.withGeminiProtocol(h.geminiModels, false)
	return func(w http.ResponseWriter, r *http.Request) {
		if geminiDiscoveryHint(r) {
			google(w, r)
			return
		}
		standard(w, r)
	}
}

func (h *Handler) unsupportedGeminiTransport(w http.ResponseWriter, _ *http.Request) {
	writeOpenAIError(w, http.StatusNotImplemented, "not_supported", "Gemini Interactions is not supported; select generateContent explicitly")
}
