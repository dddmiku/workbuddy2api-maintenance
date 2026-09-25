// ═══ 更新日志 ═══
// 2026-09-26：无该域账号时视为已就绪，失败重试按 1→30 分钟退避，不再每分钟告警。
// 2026-09-26：后台预热 CN/global 模型目录，使出站能在重启后立即按 maxOutputTokens 补齐输出预算。
package server

import (
	"context"
	"log"
	"time"
)

const (
	// modelCatalogWarmInterval 小于目录缓存 TTL（1h），保证目录在过期前刷新。
	modelCatalogWarmInterval = 30 * time.Minute
	// modelCatalogRetryInterval 目录尚未加载成功（无可用账号、上游暂时失败）时的重试间隔。
	modelCatalogRetryInterval = time.Minute
)

// WarmModelCatalogs 持续保持模型目录可用，直到 ctx 取消。
//
// 输出预算补齐依赖目录里的 maxOutputTokens；此前目录只在有人调用 /v1/models 时加载，
// 重启后到第一次列模型之前，出站请求都不带预算，上游按自身默认预留（deepseek-v4.1-flash
// 实测 384000），长会话会提前超窗。预热复用 modelList 的缓存与失败负缓存，不额外打上游。
func (h *Handler) WarmModelCatalogs(ctx context.Context) {
	if h.cfg.Upstream == nil || h.cfg.Pool == nil {
		return
	}
	timer := time.NewTimer(5 * time.Second) // 等账号池与热更新交接稳定后再探测
	defer timer.Stop()
	retry := modelCatalogRetryInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		models := len(h.modelList())
		loaded := h.realmCatalogReady("cn") && (!h.cfg.GlobalEnabled || h.realmCatalogReady("global"))
		next := modelCatalogWarmInterval
		if loaded {
			retry = modelCatalogRetryInterval
		} else {
			// 失败退避（1 分钟起翻倍，最长 30 分钟），避免上游目录接口故障期间每分钟告警。
			next = retry
			retry = min(retry*2, modelCatalogWarmInterval)
			log.Printf("WARN: [server] model catalog warm-up incomplete (models=%d); retrying in %s", models, next)
		}
		timer.Reset(next)
	}
}

// realmCatalogReady 该域目录已加载，或池中根本没有该域账号（无从加载，也无需补预算）。
func (h *Handler) realmCatalogReady(realm string) bool {
	if h.cfg.Upstream.ModelCatalogLoaded(realm) {
		return true
	}
	return h.cfg.Pool.PickExcludingForRealm(nil, "", realm) == nil
}
