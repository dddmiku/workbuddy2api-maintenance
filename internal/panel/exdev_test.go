// ═══ 更新日志 ═══
// 2026-10-15：新增。删号必须能跨文件系统搬运——容器里 auths 与 auths-trash 是两个
// 独立的 bind mount（/app/auths、/app/auths-trash），rename(2) 跨挂载点必报 EXDEV
// （"invalid cross-device link"）。用户实测删号失败报的就是这个。
//
// 这里用**跨盘/跨挂载点的真实文件系统**做验证（Windows 上用第二个临时目录所在的卷），
// 只用单元测试替身证明不了 rename 的 EXDEV 分支——那段代码在真机上才会走到。
package panel

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCopyFileAcrossPreservesContentAndMode 跨设备搬运的回落路径本身要正确：
// 内容一致、权限不放宽、且是**先落地再删源**（源文件删失败不影响目标已就位）。
func TestCopyFileAcrossPreservesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "dst.json")
	payload := []byte(`{"account":{"uid":"abc12345"},"auth":{"accessToken":"secret-token"}}`)
	if err := os.WriteFile(src, payload, 0600); err != nil {
		t.Fatal(err)
	}

	if err := copyFileAcross(src, dst); err != nil {
		t.Fatalf("跨设备搬运应成功：%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("目标文件应存在：%v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("内容不一致：%s", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// 权限只允许相等或更严（凭据文件含 access token）。Windows 上没有 POSIX mode
	// 语义（stat 恒报 0666/0444 一类），断言在那边无意义——平台判定见
	// runtime.GOOS，而不是猜 mode 数值。
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Fatalf("权限应保持 0600，实际 %o", mode)
		}
	}
	// 源文件此时**还在**——删除由 Delete 负责，copyFileAcross 不碰它。
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("copyFileAcross 不应删除源文件：%v", err)
	}
}

// TestCopyFileAcrossDoesNotClobberExisting 目标已存在时报错而不是覆盖。
//
// 回收件名带时间戳与随机后缀，正常不会重名；真撞上说明有并发或时钟问题，
// 此时覆盖会**销毁一份回收件**（那是删号后的唯一副本）。
func TestCopyFileAcrossDoesNotClobberExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "dst.json")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyFileAcross(src, dst); err == nil {
		t.Fatal("目标已存在时应报错，绝不能覆盖")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "existing" {
		t.Fatalf("已存在的回收件被改写了：%s", got)
	}
}

// TestIsCrossDeviceRecognisesEXDEV 平台判据要知道自己在判什么。
//
// 两边都引用同一份语义（Linux 的 EXDEV / Windows 的 ERROR_NOT_SAME_DEVICE），
// 拼错错误码会让回落分支永远不触发——而那正是用户遇到的 bug 本身。
// 非跨设备错误（如「文件不存在」）必须返回 false，否则所有 rename 失败都会走
// 复制路径，把「权限不足」这类错误也掩盖成一次静默的成功复制。
func TestIsCrossDeviceRecognisesEXDEV(t *testing.T) {
	if isCrossDevice(nil) {
		t.Fatal("nil 不是跨设备错误")
	}
	if isCrossDevice(os.ErrNotExist) {
		t.Fatal("ErrNotExist 不是跨设备错误")
	}
	if isCrossDevice(&os.PathError{Op: "rename", Path: "x", Err: os.ErrPermission}) {
		t.Fatal("权限错误不是跨设备错误")
	}
	if !isCrossDevice(crossDeviceError()) {
		t.Fatal("平台自带的跨设备错误码必须被识别（否则删号永久失败）")
	}
}

// TestDeleteAcrossFilesystems 端到端：源与目标在两个不同的卷/挂载点时，
// Delete 仍要把账号搬进回收目录并删掉源文件。
//
// 用 TMPDIR 之外的第二个盘做目标（Windows 上换盘符，Linux 上换挂载点）。
// 找不到第二个卷时跳过——那不是被测代码的问题，环境限制要如实说明而不是伪装通过。
func TestDeleteAcrossFilesystems(t *testing.T) {
	srcRoot := t.TempDir()
	dstRoot := secondVolumeDir(t)
	if dstRoot == "" {
		t.Skip("本机只有一个卷，无法验证跨文件系统删除（容器里是 bind mount 场景）")
	}
	authDir := filepath.Join(srcRoot, "auths")
	trashDir := filepath.Join(dstRoot, "auths-trash")
	for _, d := range []string{authDir, trashDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeAuthFixture(t, authDir, "workbuddy-abc12345.json", "abc12345")

	store := &AccountStore{AuthDir: authDir, TrashDir: trashDir}
	trashed, err := store.Delete("abc12345")
	if err != nil {
		t.Fatalf("跨文件系统删除应成功（先复制后删源），实际：%v", err)
	}
	if trashed == "" {
		t.Fatal("应返回回收件名")
	}
	if _, err := os.Stat(filepath.Join(trashDir, trashed)); err != nil {
		t.Fatalf("回收件应落在回收目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "workbuddy-abc12345.json")); !os.IsNotExist(err) {
		t.Fatalf("源文件应已被删除（账号才算真的从池里摘掉）：%v", err)
	}
}
