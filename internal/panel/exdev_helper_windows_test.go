//go:build windows

// 2026-10-15：测试辅助——给出平台自带的「跨设备」错误码，以及本机第二个卷。
package panel

import (
	"os"
)

// crossDeviceError 返回平台认定的跨设备移动错误（Windows 的 ERROR_NOT_SAME_DEVICE）。
func crossDeviceError() error { return errNotSameDevice }

// secondVolumeDir 返回一个与 t.TempDir() **不在同一卷**的可写目录；找不到时返回空串。
//
// 判据不是「盘符不同」也不是「卷序列号不同」，而是**直接试一次 os.Rename**——
// 那正是被测代码要处理的语义。盘符/序列号都只是代理指标，会有挂载点、subst、
// symbolic link 之类的假象；试一次真操作则是同一个事实来源，不可能判错。
//
// 目录名以 .probe 开头，试完即删，不留残留。
func secondVolumeDir(t interface{ TempDir() string }) string {
	base := t.TempDir()
	probeSrc := base + `\.crossdev-probe`
	if err := os.WriteFile(probeSrc, []byte("probe"), 0600); err != nil {
		return ""
	}
	defer func() { _ = os.Remove(probeSrc) }()
	for _, letter := range "DEFGHIJKLMNOPQRSTUVWXYZ" {
		root := string(letter) + `:\`
		if _, statErr := os.Stat(root); statErr != nil {
			continue
		}
		dir, mkErr := os.MkdirTemp(root, "wb2api-crossdev-")
		if mkErr != nil {
			continue
		}
		probeDst := dir + `\.crossdev-probe`
		renameErr := os.Rename(probeSrc, probeDst)
		// 无论结果如何都要把探针清掉（成功时它在目标盘上）。
		_ = os.Remove(probeDst)
		if renameErr == nil {
			// 同一卷：这个盘符不是我们要找的第二个卷，清掉刚建的目录继续找。
			_ = os.Remove(dir)
			// 探针被 rename 走了，补回来给下一轮用。
			if err := os.WriteFile(probeSrc, []byte("probe"), 0600); err != nil {
				return ""
			}
			continue
		}
		if isCrossDevice(renameErr) {
			return dir
		}
		// 别的错（权限、只读盘）——不是可用的第二个卷。
		_ = os.Remove(dir)
	}
	return ""
}
