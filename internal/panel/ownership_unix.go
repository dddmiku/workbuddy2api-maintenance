//go:build !windows

// ═══ 更新日志 ═══
// 2026-10-13：初版——账号文件落盘后的属主/权限收口，与 panel/app.py 的 chown_app 同口径。
package panel

import (
	"log"
	"os"
)

// gatewayUID / gatewayGID 是网关进程在容器里的运行属主（Dockerfile 里固定 10001）。
//
// 为什么必须由面板来改属主：面板以 root 运行、网关以 10001 运行，账号文件若停在
// root:0600，网关读不到会被**静默跳过**（2026-09-23 实测：加号后只加载 23 个账号，
// 10 秒后才恢复 24）。所以写入路径上是「先 chown 再 rename」，落地瞬间就必须是
// 网关可读的形态。
const (
	gatewayUID = 10001
	gatewayGID = 10001
)

// chownApp 把网关侧文件归属到 10001:10001 并收紧到 0600。
//
// 与 Python 版的差异只有一处，且是有意的：**非 root 时跳过 chown**。
// chown 到别的属主需要 CAP_CHOWN，非 root 进程必然失败；桌面版与安卓版跑在
// 用户自己的 uid 下，文件本来就归自己所有，再 chown 到 10001 既做不到也没有意义，
// 只会让每次加号都打一行吓人的告警。容器部署（面板是 root）行为与 Python 完全一致。
//
// 失败不静默：chmod 失败说明挂载卷或权限模型有变，必须留痕——否则又会回到
// 「加号成功但号没生效」那种没有解释的故障。
func chownApp(path string) {
	if os.Geteuid() == 0 {
		if err := os.Chown(path, gatewayUID, gatewayGID); err != nil {
			log.Printf("chown %s failed: %v", path, err)
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		log.Printf("chmod %s failed: %v", path, err)
	}
}
