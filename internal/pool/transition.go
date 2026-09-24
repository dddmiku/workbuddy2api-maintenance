// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由四个正交维度决定：禁用(disabled)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)。维度之间以「迁移原语」收拢，
// 禁止在其他文件散写这些字段——所有入口（applyErrorPolicy / refresh / keepalive /
// 签到 / 选号）对状态的改动都必须经本文件的原语或经 Cooldown/NoteError/NoteSuccess 等
// 封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← 模型限额或负缓存；到期、禁用或对应模型成功时清理
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/禁用清理
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 仅供禁用清理；签到余额恢复只清余额不足的账号级冷却。
//
// ═══ 更新日志 ═══
// 2026-09-24：签到余额恢复不再覆盖独立的软限流、模型日限额或模型不可用记录。
// 2026-09-18：记录禁用与解冻的显式字段意图，跨实例合并时不把同值赋值误判为旧快照。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 疑点 4 修正：旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，
// 会出现「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个
// 先被硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.markStateFieldsLocked(e.a.UID, "disabled", "reason", "until", "cool_kind", "soft_streak", "model_cooldowns")
}

// reviveCoolingLocked 更新余额，并只解除由余额不足引起的账号冷却。
// 签到不证明模型配额、频率限制或聊天服务恢复，其余状态保持独立。
// 调用方必须已持有 p.mu，且确认余额为正、账号未禁用。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	p.markStateFieldsLocked(e.a.UID, "credits")
	if e.coolKind == CoolHard {
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
		p.markStateFieldsLocked(e.a.UID, "reason", "until", "cool_kind")
	}
}
