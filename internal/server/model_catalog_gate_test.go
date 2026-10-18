// ═══ 更新日志 ═══
// 2026-10-18：11102 分流——模型不在上游目录里时不再写 (账号,模型) 负缓存。
//             用户指出客户端模型列表来自上游接口，网关应当按同一份事实判定，
//             而不是靠逐账号试错去发现「这个模型已经下线」。
package server

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// setCatalogForTest 注入模型目录缓存，并在测试结束时还原。
//
// 目录是包级全局（dynamicModelsCache 由 /v1/models 刷新驱动），测试必须自己
// 播种与清理，否则用例之间互相污染——这也是这些用例都串行跑的原因。
func setCatalogForTest(t *testing.T, ids ...string) {
	t.Helper()
	infos := make([]upstream.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, upstream.ModelInfo{ID: id})
	}
	dynamicModelsCache.Lock()
	prevIDs, prevFetched := dynamicModelsCache.ids, dynamicModelsCache.fetched
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = prevIDs, prevFetched
		dynamicModelsCache.Unlock()
	})
}

// clearCatalogForTest 把目录置空（模拟启动初期未拉取成功）。
func clearCatalogForTest(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	prevIDs, prevFetched := dynamicModelsCache.ids, dynamicModelsCache.fetched
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = prevIDs, prevFetched
		dynamicModelsCache.Unlock()
	})
}

func TestModelCatalogStateClassifiesRetiredModel(t *testing.T) {
	setCatalogForTest(t, "glm-5.3", "deepseek-v4.1-flash")
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})

	if got := h.modelCatalogState("glm-5.3"); got != modelInCatalog {
		t.Fatalf("glm-5.3 在目录里，应为 modelInCatalog，got=%v", got)
	}
	// 已下线的旧名字（实测 CN 台账里的真实取值）。
	if got := h.modelCatalogState("glm-4.6"); got != modelRetired {
		t.Fatalf("glm-4.6 不在目录里，应为 modelRetired，got=%v", got)
	}
	if got := h.modelCatalogState("kimi-k2-thinking"); got != modelRetired {
		t.Fatalf("kimi-k2-thinking 不在目录里，应为 modelRetired，got=%v", got)
	}
}

func TestModelCatalogStateUnknownWhenCatalogEmpty(t *testing.T) {
	clearCatalogForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	// 目录未就绪时不能把「不知道」当成「不存在」——否则会漏记真实的账号权限问题。
	if got := h.modelCatalogState("anything"); got != modelCatalogUnknown {
		t.Fatalf("目录为空时应为 modelCatalogUnknown，got=%v", got)
	}
	if got := h.modelCatalogState(""); got != modelCatalogUnknown {
		t.Fatalf("空模型名应为 modelCatalogUnknown，got=%v", got)
	}
}

func TestModelCatalogStateMatchesAliasSpelling(t *testing.T) {
	setCatalogForTest(t, "glm-5.3")
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	// 客户端可能带 realm 前缀，目录里是裸名（或反之）；两种拼写都要算命中。
	if got := h.modelCatalogState("cn:glm-5.3"); got != modelInCatalog {
		t.Fatalf("带 realm 前缀的同一模型应命中，got=%v", got)
	}
}

// TestRetiredModelDoesNotWriteAccountNegativeCache 是本组的核心断言：
// 11102 + 模型已下线 → 不写任何 (账号,模型) 避让，台账保持干净。
func TestRetiredModelDoesNotWriteAccountNegativeCache(t *testing.T) {
	setCatalogForTest(t, "glm-5.3")
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}})

	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, "", "glm-4.6")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Fatalf("已下线模型不该写账号级避让，got=%+v", st.RateLimitedModels)
	}
}

// TestInCatalogModelStillWritesNegativeCache 保证修复没有过度收窄：
// 目录里**有**这个模型时，11102 仍是账号级权限差异，必须照旧避让。
func TestInCatalogModelStillWritesNegativeCache(t *testing.T) {
	setCatalogForTest(t, "glm-5.3")
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}})

	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, "", "glm-5.3")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("目录内的模型仍应记避让，got=%+v", st.RateLimitedModels)
	}
	if !strings.Contains(st.RateLimitedModels[0].Reason, "11102") {
		t.Fatalf("reason 应标明 11102：%+v", st.RateLimitedModels[0])
	}
}

// TestUnknownCatalogStillWritesNegativeCache 目录未就绪时按保守方向处理：
// 照旧记避让（宁可多避让一个模型，也不要让真正无权限的账号被反复选中重试）。
func TestUnknownCatalogStillWritesNegativeCache(t *testing.T) {
	clearCatalogForTest(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}})

	h.applyErrorPolicy("u1", upstream.ErrModelBlocked, "", "whatever")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("目录未知时应保守记录避让，got=%+v", st.RateLimitedModels)
	}
}

// TestRetiredModelLogIsRateLimited 同一模型名一小时内只记一行日志。
func TestRetiredModelLogIsRateLimited(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: &upstream.Client{}})
	h.noteRetiredModel("glm-4.6")
	h.retiredModelMu.Lock()
	n := len(h.retiredModelSeen)
	first := h.retiredModelSeen["glm-4.6"]
	h.retiredModelMu.Unlock()
	if n != 1 {
		t.Fatalf("首次应记录一条，got=%d", n)
	}
	// 再次调用不应改变记录时刻（被去重跳过）。
	h.noteRetiredModel("glm-4.6")
	h.retiredModelMu.Lock()
	second := h.retiredModelSeen["glm-4.6"]
	h.retiredModelMu.Unlock()
	if !second.Equal(first) {
		t.Fatalf("一小时内重复调用不该刷新记录时刻：%v -> %v", first, second)
	}
}
