// device.go — Trae 出站设备指纹。
//
// Trae2api-cn 用全局随机值 + 9074 风控时按账号轮换（_CHECKIN_DEVICE_IDS）。
// 网关侧按 UID 派生稳定哈希：同一账号跨请求/重启恒同值（等价于 Trae2api-cn 的
// "首次随机后固定"），且无需可变状态。
//
// 9074 的设备号轮换已实现（2026-10-11）：签到侧用 checkinDeviceIDGen(uid, gen)
// 按代数派生，命中 9074 时由 scheduler 把代数 +1 重试一次。实测同一账号同一 JWT，
// 第 0 代 9074、第 1 代立刻 success——该信号是设备维度而非账号维度。
package trae

import (
	"crypto/sha256"
	"encoding/hex"
)

// deviceID 从 (uid, purpose) 派生一个稳定指纹值。
//
// 设备指纹格式对齐 Trae 客户端形态：
//   - device/machine：32 位 hex（对齐 IDE 客户端的 uuid 形态）
//   - request：每次独立——但这里仍用派生值保证可测试性；真正的逐请求随机性
//     由 ChatStream 调用点注入（见 stream.go），此函数只服务设备/机器指纹。
func deviceID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("trae-device:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:16])
}
