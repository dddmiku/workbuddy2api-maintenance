// ═══ 更新日志 ═══
// 2026-10-13：初版——向网关转发读/写请求（与 panel/app.py 的 gateway_get/post/post_json 同口径）。
//
// 两条通道，优先级明确：
//
//  1. **本机管理 socket**（首选）。它只在本机可达、权限 0600，面板经它拿到的是
//     管理员视图（全量用量、热更新、账号复活）。能用就一定用它。
//  2. **HTTP + api_key**（回落）。socket 不可用时（老部署把面板单独跑在别的容器里、
//     或配置里没写 api_keys_socket）退到这条。它是**降级路径**，能力受调用密钥的
//     权限限制，所以只在第一条走不通时才用。
//
// 返回值统一是 (ok, payload)：面板的调用点只关心「成没成」和「信封里写了什么」，
// 让它们各自处理 error 只会把同一段判断抄十几遍。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// GatewayClient 访问网关的内部接口。
type GatewayClient struct {
	Management *ManagementClient
	// BaseURL 是网关的 HTTP 地址（回落路径用），例如 http://127.0.0.1:7863。
	BaseURL string
	// APIKey 是 config.json 里的调用密钥（回落路径的凭证）。
	APIKey string
	// HTTPClient 可被测试替换。
	HTTPClient *http.Client
}

func (g *GatewayClient) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Get 读网关接口；连不上或响应无法解析时返回 nil。
//
// 与 Python 版一致：读接口的失败对面板来说是「这个卡片暂时没数据」，
// 而不是错误——返回 nil 让调用方走各自的空态展示。
func (g *GatewayClient) Get(ctx context.Context, path string) map[string]any {
	if g.Management != nil && g.Management.ManagementEnabled() {
		if code, payload := g.Management.Request(ctx, http.MethodGet, path, nil); code < 400 {
			return payload
		}
		return nil
	}
	if g.BaseURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+path, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	return g.doJSON(req)
}

// Post 发一个空体 POST；返回 (ok, payload)。
func (g *GatewayClient) Post(ctx context.Context, path string) (bool, map[string]any) {
	return g.send(ctx, path, nil, false)
}

// PostJSON 发一个带 JSON 请求体的 POST；返回 (ok, payload)。
//
// 与 Post 分开（而不是给它加个可选 body）：空体与带体在 socket 与 HTTP 两条路径上
// 的写法并不一样，合并会让「本来该是空体的调用」多出一个 Content-Length: 0 的差异。
func (g *GatewayClient) PostJSON(ctx context.Context, path string, payload map[string]any) (bool, map[string]any) {
	return g.send(ctx, path, payload, true)
}

func (g *GatewayClient) send(ctx context.Context, path string, body map[string]any, withBody bool) (bool, map[string]any) {
	if g.Management != nil && g.Management.ManagementEnabled() {
		var payload any
		if withBody {
			payload = body
		}
		code, result := g.Management.Request(ctx, http.MethodPost, path, payload)
		if code >= 400 {
			return false, result
		}
		return true, result
	}
	if g.BaseURL == "" {
		return false, map[string]any{"ok": false, "message": "无法读取网关配置"}
	}
	var reader io.Reader
	if withBody {
		raw, err := json.Marshal(body)
		if err != nil {
			return false, map[string]any{"error": map[string]any{"message": "连不上网关：" + err.Error()}}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+path, reader)
	if err != nil {
		return false, map[string]any{"error": map[string]any{"message": "连不上网关：" + err.Error()}}
	}
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	if withBody {
		req.Header.Set("Content-Type", "application/json")
	}
	result := g.doJSON(req)
	if result == nil {
		return false, map[string]any{"error": map[string]any{"message": "连不上网关"}}
	}
	// 回落路径的失败码要从 HTTP 状态里看：管理 socket 会给出码，HTTP 路径不会，
	// 所以这里把「信封里明确写了 ok=false」也当成失败。
	if ok, present := result["ok"].(bool); present && !ok {
		return false, result
	}
	return true, result
}

// doJSON 发请求并把响应体解析成对象；任何一步失败都返回 nil。
func (g *GatewayClient) doJSON(req *http.Request) map[string]any {
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, managementResponseLimit+1))
	if err != nil {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil
	}
	return result
}

// LoadAPIKey 从 config.json 读调用密钥；读不到返回空串（回落路径会因此免鉴权）。
func LoadAPIKey(configPath string) string {
	return configString(configPath, "api_key")
}

// normalizeBaseURL 去掉结尾斜杠，避免拼出 //v1/models 这种地址。
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}
