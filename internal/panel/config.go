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
	"net"
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

// ═══ 局域网访问（2026-10-15）════════════════════════════════════════════════

// lanListenHost 局域网开启时的监听主机（全部网卡）。
const lanListenHost = "0.0.0.0"

// loopbackListenHost 默认（关闭局域网）时的监听主机——与安卓/桌面默认配置同源
// （internal/android/layout.go 的 DefaultConfig 也写 127.0.0.1:7863）。
const loopbackListenHost = "127.0.0.1"

// listenPortOf 从 listen 值解析端口；解析不到回落 7863。
//
// 接受 "7863"、":7863"、"127.0.0.1:7863"、"0.0.0.0:7864" 四种形态。
func listenPortOf(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return "7863"
	}
	if i := strings.LastIndex(listen, ":"); i >= 0 && i < len(listen)-1 {
		if port := listen[i+1:]; strings.Trim(port, "0123456789") == "" {
			return port
		}
		return "7863"
	}
	if strings.Trim(listen, "0123456789") == "" {
		return listen
	}
	return "7863"
}

// LANAccessState 读当前局域网访问状态：enabled = listen 绑的不是回环；
// allowCIDRs = admin_allow_cidrs 的原始值（空 = 未配置）。
//
// 判定口径「非回环即开启」而不是「等于 0.0.0.0」：运维手写的 listen 可能是
// 具体网卡 IP（如 192.168.5.65:7863），那同样是局域网可达，开关应如实显示开。
func LANAccessState(configPath string) (enabled bool, allowCIDRs string) {
	object, err := readConfigObject(configPath)
	if err != nil {
		return false, ""
	}
	listen, _ := jsonStringOf(object, "listen")
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || strings.TrimSpace(listen) == "" {
		host = ""
	}
	enabled = host != "" && host != loopbackListenHost && host != "localhost" &&
		host != "::1" && !strings.EqualFold(host, "[::1]")
	cidrs, _ := jsonStringOf(object, "admin_allow_cidrs")
	return enabled, cidrs
}

// jsonStringOf 从保序对象读顶层字符串键。
func jsonStringOf(object *orderedObject, key string) (string, bool) {
	if object == nil {
		return "", false
	}
	value, ok := object.Get(key)
	if !ok {
		return "", false
	}
	text, isString := value.(string)
	if !isString {
		return "", false
	}
	return strings.TrimSpace(text), true
}

// SetLANAccess 开/关局域网访问（写 config.json，重启后生效）。
//
// 开启的前提是 api_key 非空或已配置密钥库（api_keys_file 指向的文件存在）：
// listen 放开后面板与 API 一起暴露到局域网，而空 api_key = 任何同网段的人都能
// 直接调用账号池。密钥库形态下按文件存在判定（面板密钥页建的第一把密钥就落它），
// 不再读内容——内容校验是网关启动时的事。
//
// admin_allow_cidrs：非空时写入（API 与面板两侧都会按它 403 白名单外的来源）；
// 空串时删除该键（不限网段）。非法 CIDR 不在这里报错——解析侧（panel/throttle.go
// 的 parseNetworks）对非法项跳过并打日志，网关仍能启动；真要拦也是配置校验的事。
//
// 返回 (ok, message)。已是目标状态返回「状态未变化」（调用方据此跳过重启）。
func SetLANAccess(configPath string, enabled bool, allowCIDRs string) (bool, string) {
	object, err := readConfigObject(configPath)
	if err != nil {
		return false, fmt.Sprintf("读取 config.json 失败：%s", err)
	}
	listen, _ := jsonStringOf(object, "listen")
	port := listenPortOf(listen)

	if enabled {
		// 安全闸门：没有可用鉴权就拒绝放开监听——不写文件、不重启。
		apiKey, _ := jsonStringOf(object, "api_key")
		keysFile, _ := jsonStringOf(object, "api_keys_file")
		keysExist := keysFile != "" && fileExists(keysFile)
		if apiKey == "" && !keysExist {
			return false, "请先在「密钥」页创建一把调用密钥，再开启局域网访问：放开监听后面板与 API 一起暴露到局域网，空密钥等于任何同网段的人都能直接调用账号池"
		}
	}

	// 关闭时忽略传入的 CIDR、直接清空白名单：关闭的语义是「回到只本机」，
	// 白名单只在开启时有意义；留着旧值会在下次开启时意外复用。
	want := loopbackListenHost + ":" + port
	wantCIDRs := ""
	if enabled {
		want = lanListenHost + ":" + port
		wantCIDRs = strings.TrimSpace(allowCIDRs)
	}
	current, _ := jsonStringOf(object, "listen")
	currentCIDRs, _ := jsonStringOf(object, "admin_allow_cidrs")
	if strings.TrimSpace(current) == want && currentCIDRs == wantCIDRs {
		return true, "状态未变化"
	}
	object.Set("listen", want)
	if wantCIDRs == "" {
		object.Delete("admin_allow_cidrs")
	} else {
		object.Set("admin_allow_cidrs", wantCIDRs)
	}
	if err := writeConfigObject(configPath, object); err != nil {
		return false, fmt.Sprintf("写入 config.json 失败：%s", err)
	}
	return true, "已保存（重启后生效）"
}

