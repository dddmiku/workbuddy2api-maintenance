// ═══ 更新日志 ═══
// 2026-10-07：新增「健康账号全被在途名额占满」时的有界等待，把并发峰值下的本地 503
// 改成短暂排队（实测 9 分钟内 294 次请求在本地被拒）。
package server

import (
	"context"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// slotWaitable 报告一次选号失败是否**只**因为「健康账号全被在途名额占满」。
//
// 判据取自选号器自己记录的事实：healthy 计入所有通过 realm / 禁用 / 冷却检查的
// 账号，available 是它们当中再通过在途检查的那部分——两者之间只有 in_flight_full
// 一个过滤点，所以「healthy > 0 且 available == 0」精确等价于「有号可用、只是全忙」。
//
// 为什么要区分这两种「选不到号」：
//   - 全忙是**秒级**瞬时状态（既有请求一结束名额就释放），值得短暂排队；
//   - 无健康账号（全禁用 / 全冷却）在几秒内不会改变，等待只会白让客户端等，
//     必须继续 fail fast。
//
// 2026-10-07 实测：27 个号的池子里 7 个被内容审核标记停用、13 个撞上游 6004 冷却、
// 5 个 realm 不符，只剩 2 个健康号共 6 个在途名额；一个客户端的并发峰值就能把它们
// 全占满，于是 9 分钟内 294 次请求在**本地**被拒（attempt_count=0，请求根本没发到
// 上游），而同期账号一直在间歇成功——名额确实几百毫秒到几秒就释放。
func slotWaitable(decision pool.Decision) bool {
	return decision.StageCounts["healthy"] > 0 && decision.StageCounts["available"] == 0
}

// waitForSlot 在池中健康账号全被在途名额占满时，有界等待一个名额释放后重选。
//
// 返回最后一次选号的结果。等待预算耗尽（deadline）、上层 ctx 结束、或失败原因不再
// 是「全忙」时返回 nil，由调用方按原有语义回 503——等待只推迟判定，不改变判定。
//
// 循环顺序是「先取等待通道 → 再选号 → 最后等待」，三者不能颠倒：若先选号再取通道，
// 在两步之间发生的名额释放会关闭**旧**通道，而调用方持有的是新通道，于是这次释放
// 被永久错过、只能干等到 deadline——那恰好是全忙场景下最坏的延迟。
// 先取通道则保证两步之间的任何释放都会让随后的等待立即返回。
func (h *Handler) waitForSlot(ctx context.Context, tried map[string]bool, model, realm string, deadline time.Time) (*auth.Auth, pool.Decision) {
	for {
		freed := h.cfg.Pool.SlotFreed()
		var decision pool.Decision
		acct, decision := h.cfg.Pool.PickExcludingForRealmWithDecision(tried, model, realm)
		if acct != nil || !slotWaitable(decision) {
			return acct, decision
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, decision
		}
		timer := time.NewTimer(remaining)
		select {
		case <-freed:
			// 有账号腾出名额（未必是本请求能用的那个）：重选一次再判断。
			timer.Stop()
		case <-timer.C:
			return nil, decision
		case <-ctx.Done():
			// 客户端已断开：不必再等，调用方会在下一轮循环用 ctx.Err() 收尾。
			timer.Stop()
			return nil, decision
		}
	}
}
