package server

import (
	"strings"

	"workbuddy2api/internal/auth"
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
		return auth.RealmCN, model
	}
	prefix := model[:idx]
	if !knownRealm(prefix) {
		return auth.RealmCN, model
	}
	return prefix, model[idx+1:]
}

// knownRealm 报告前缀是否为网关认识的 realm。新增域时只改这里。
func knownRealm(prefix string) bool {
	for _, realm := range realmNames() {
		if prefix == realm {
			return true
		}
	}
	return false
}

// realmNames 返回网关全部 realm，供 /healthz、/status、模型目录等展示层遍历。
//
// 这些地方原先各自硬编码 {"cn","global"} 两个键，接入 trae 时全部漏掉——
// 结果是 trae 域的可达性对负载均衡与运维完全不可见（trae 全挂时 /healthz 仍 200）。
// 集中一处，新增域时不再需要满仓找。
func realmNames() []string {
	return []string{auth.RealmCN, auth.RealmGlobal, trae.Realm}
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
