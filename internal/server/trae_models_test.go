package server

import (
	"strings"
	"testing"

	"workbuddy2api/internal/upstream/trae"
)

// TestModelListPublishesTraeCatalogWhenServable 确认 trae 域模型会出现在模型列表里。
//
// 这是「面板能选到 trae 模型 → 密钥能绑定 → 请求能路由」这条链路的入口：
// 列表里没有，用户就无从绑定；而 trae 域只在池里真有可服务账号时才公布
// （没有号却列出模型，客户端会选到一个永远选不到号的模型名）。
func TestModelListPublishesTraeCatalogWhenServable(t *testing.T) {
	catalog := trae.ModelIDs()
	if len(catalog) == 0 {
		t.Fatal("trae catalog is empty")
	}
	// 目录本身必须自洽：别名映射的目标也在清单里（否则按真实名请求会选不中）。
	seen := map[string]bool{}
	for _, id := range catalog {
		seen[id] = true
	}
	for alias, target := range trae.ModelAliases() {
		if !seen[target] {
			t.Errorf("alias %q -> %q but %q is not published", alias, target, target)
		}
	}
	// 拼出的 id 必须是 "<realm>:<model>" 形状，与 resolveModel 对称。
	for _, id := range catalog {
		full := trae.Realm + ":" + id
		realm, bare := resolveModel(full)
		if realm != trae.Realm {
			t.Errorf("resolveModel(%q) realm=%q want %q", full, realm, trae.Realm)
		}
		if bare != id {
			t.Errorf("resolveModel(%q) bare=%q want %q", full, bare, id)
		}
		if !strings.HasPrefix(full, "trae:") {
			t.Errorf("published id %q is not prefixed", full)
		}
	}
}
