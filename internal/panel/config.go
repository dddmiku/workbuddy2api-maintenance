// ═══ 更新日志 ═══
// 2026-10-13：初版——config.json 的读写（任务开关、池参数、密钥管理路径解析）。
//
// config.json 是运维手写的文件，面板只改其中一两个键。因此这里的读写必须：
//
//  1. **保序**：用 jsonorder.go 的保序对象，改一个开关不会把整份文件重排
//     （map 序列化会按键名排序，diff 里全是噪声，运维再也不敢让工具碰这个文件）。
//  2. **保权限**：文件里含调用密钥。重写后要恢复原文件的 mode 与属主，否则一次
//     「改个开关」就把它变成 world-readable。
//  3. **原子**：临时文件 + rename，中途被杀不会留下半个 JSON。
//
// 容器里 config.json 是只读挂进去的，所以改完必须重启网关才生效——由调用方负责，
// 这里只负责把文件写对，并在返回值里说明「已保存（重启后生效）」。
package panel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// taskEnableKey 是任务标识 → config.json 里开关字段的映射，与面板白名单同一份。
//
// 漏登记一个任务的后果是「网关侧任务存在、面板却报『任务标识不正确』」——面板的
// 白名单把运维挡在门外。trae_check 就是照这个教训补进来的：Trae 是独立站点，
// 签到有自己的开关（scheduler 的 traeCheckAll），不能和 WorkBuddy 的签到合并。
var taskEnableKey = map[string]string{
	"checkin":    "checkin_enabled",
	"trae_check": "trae_check_enabled",
	"travel":     "travel_enabled",
	"activity":   "activity_enabled",
	"keepalive":  "keepalive_enabled",
	"school":     "school_enabled",
	"cat":        "cat_enabled",
	// growth：成长中心（2026-10-14 由原 redeem/lottery/makeup 三条合并）。漏登记会让
	// 面板「跑一次 / 开关」报「任务标识不正确」——网关侧任务已存在，只是白名单挡在门外。
	"growth": "growth_enabled",
	// balance：余额刷新（纯读）。漏登记会让面板「跑一次/开关」报「任务标识不正确」。
	"balance": "balance_enabled",
}

// TaskKeys 返回面板接受的任务标识（顺序固定，便于测试与日志）。
func TaskKeys() []string {
	// 顺序与 app.py 的 TASK_ENABLE_KEY 字面量一致；Go 的 map 无序，所以显式列出。
	return []string{"checkin", "trae_check", "travel", "activity", "keepalive",
		"school", "cat", "growth", "balance"}
}

// KnownTask 判断任务标识是否在白名单内。
func KnownTask(key string) bool {
	_, ok := taskEnableKey[key]
	return ok
}

// readConfigObject 读 config.json 成保序对象。
func readConfigObject(path string) (*orderedObject, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value, err := parseOrderedJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(*orderedObject)
	if !ok {
		return nil, errors.New("config.json 顶层不是 JSON 对象")
	}
	return object, nil
}

// SetTaskEnabled 改 config.json 的 schedule.<key>_enabled。返回 (ok, message)。
//
// 保留原文件的缩进与键序，只在目标键上动刀。已是目标值时返回 ErrNoChange 对应的
// 文案「状态未变化」——调用方据此跳过重启（重启一次要几十秒，白重启很扰民）。
func SetTaskEnabled(configPath, key string, enabled bool) (bool, string) {
	field, ok := taskEnableKey[key]
	if !ok {
		return false, fmt.Sprintf("未知任务：%s", key)
	}
	object, err := readConfigObject(configPath)
	if err != nil {
		return false, fmt.Sprintf("读取 config.json 失败：%s", err)
	}
	schedule := orderedChild(object, "schedule")
	if schedule == nil {
		schedule = newOrderedObject()
		object.Set("schedule", schedule)
	}
	if current, exists := schedule.Get(field); exists {
		if asBool, isBool := current.(bool); isBool && asBool == enabled {
			return true, "状态未变化"
		}
	}
	schedule.Set(field, enabled)
	if err := writeConfigObject(configPath, object); err != nil {
		return false, fmt.Sprintf("写入 config.json 失败：%s", err)
	}
	return true, "已保存（重启后生效）"
}

// writeConfigObject 原子写回 config.json，并尽量恢复原有权限与属主。
func writeConfigObject(path string, object *orderedObject) error {
	raw, err := marshalOrdered(object, "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// 先记下原文件的 mode/属主：config.json 含调用密钥，重写后必须保持原权限，
	// 否则「改个开关」会把它变成 world-readable。取不到就按 0600 落盘（更严，不会放松）。
	var mode os.FileMode = 0600
	ownerUID, ownerGID := -1, -1
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
		if uid, gid, ok := fileOwner(info); ok {
			ownerUID, ownerGID = uid, gid
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这里是 no-op
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	// 恢复属主失败不阻断：面板多半就是文件属主，chown 到同一个 uid 是 no-op；
	// 真的改不动时（容器里 root 写的卷），文件仍是可读的，只是属主变了。
	if ownerUID >= 0 && os.Geteuid() == 0 {
		_ = os.Chown(tmpName, ownerUID, ownerGID)
	}
	return os.Rename(tmpName, path)
}

// configString 读 config.json 顶层字符串键；读不到返回空串。
func configString(path, key string) string {
	object, err := readConfigObject(path)
	if err != nil {
		return ""
	}
	value, ok := object.Get(key)
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// configBoolPointer 读顶层可空布尔键，区分「没写」与「显式 false」。
func configBoolPointer(path, key string) *bool {
	object, err := readConfigObject(path)
	if err != nil {
		return nil
	}
	value, ok := object.Get(key)
	if !ok {
		return nil
	}
	flag, isBool := value.(bool)
	if !isBool {
		return nil
	}
	return &flag
}

// configNestedString 读 <section>.<key> 的字符串值。
func configNestedString(path, section, key string) string {
	object, err := readConfigObject(path)
	if err != nil {
		return ""
	}
	child := orderedChild(object, section)
	if child == nil {
		return ""
	}
	value, ok := child.Get(key)
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return strings.TrimSpace(text)
}
