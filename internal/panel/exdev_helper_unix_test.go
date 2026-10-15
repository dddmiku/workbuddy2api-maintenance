//go:build !windows

// 2026-10-15：测试辅助——给出平台自带的「跨设备」错误码，以及本机第二个卷。
package panel

import (
	"os"
	"path/filepath"
	"syscall"
)

// crossDeviceError 返回平台认定的跨设备重命名错误（Linux 的 EXDEV）。
func crossDeviceError() error { return syscall.EXDEV }

// secondVolumeDir 返回一个与 t.TempDir() **不在同一文件系统**的可写目录；
// 找不到时返回空串（调用方跳过）。
//
// 遍历常见挂载点：容器里 /tmp 与 / 常是不同 overlay，/dev/shm 通常是 tmpfs。
// 判据是 st_dev 真的不同——只按路径猜会在「/tmp 与 / 同一个卷」的机器上假通过。
func secondVolumeDir(t interface{ TempDir() string }) string {
	base := t.TempDir()
	baseDev, err := deviceOf(base)
	if err != nil {
		return ""
	}
	for _, candidate := range []string{"/dev/shm", "/run", "/tmp", "/var/tmp"} {
		info, statErr := os.Stat(candidate)
		if statErr != nil || !info.IsDir() {
			continue
		}
		dev, devErr := deviceOf(candidate)
		if devErr != nil || dev == baseDev {
			continue
		}
		dir, mkErr := os.MkdirTemp(candidate, "wb2api-crossdev-")
		if mkErr != nil {
			continue
		}
		return dir
	}
	return ""
}

// deviceOf 取路径所在文件系统的设备号。
func deviceOf(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}

var _ = filepath.Join // 保持 filepath 导入（跨平台构建一致性）
