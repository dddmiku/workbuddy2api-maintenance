package server

import (
	"strings"

	"workbuddy2api/internal/upstream/trae"
)

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为已知 realm（cn / global / trae）才剥离；否则视为裸名，
// realm=cn、bare=原串。大小写敏感（前缀必须是精确的小写枚举）。
// bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if !knownRealm(prefix) {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// knownRealm 报告前缀是否为网关认识的 realm。新增域时只改这里。
func knownRealm(prefix string) bool {
	switch prefix {
	case "cn", "global", trae.Realm:
		return true
	}
	return false
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
