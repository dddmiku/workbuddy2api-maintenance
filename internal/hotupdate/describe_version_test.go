// ═══ 更新日志 ═══
// 2026-10-19：新增。锁住 git describe 后缀版本的比较语义。
package hotupdate

import (
	"testing"

	versionpkg "workbuddy2api/internal/version"
)

// 镜像 CI 注入的版本是 `v3.14.2-6-g5018cf2` 形态（git describe）。
// 旧实现按 `.` 切三段解析，第三段 `2-6-g5018cf2` 过不了 ParseUint，
// 于是退回字符串比较，把「跑着更新代码的站点」判成已是最新。
func TestParseBuildVersion(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		nums  [3]uint64
		ahead uint64
	}{
		{"v3.14.2", true, [3]uint64{3, 14, 2}, 0},
		{"3.14.2", true, [3]uint64{3, 14, 2}, 0},
		{" v3.14.2 ", true, [3]uint64{3, 14, 2}, 0},
		{"v3.14.2-6-g5018cf2", true, [3]uint64{3, 14, 2}, 6},
		{"v3.14.2-0-g5018cf2", true, [3]uint64{3, 14, 2}, 0},
		{"v3.14.2-6-g5018cf2-dirty", true, [3]uint64{3, 14, 2}, 6},
		{"v0.0.0-5018cf2", false, [3]uint64{}, 0}, // 纯 SHA 还原不出提交距离
		{"dev", false, [3]uint64{}, 0},
		{"v3.14", false, [3]uint64{}, 0},
		{"", false, [3]uint64{}, 0},
	}
	for _, c := range cases {
		got, ok := parseBuildVersion(c.in)
		if ok != c.ok {
			t.Errorf("parseBuildVersion(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.nums != c.nums || got.ahead != c.ahead {
			t.Errorf("parseBuildVersion(%q) = %v ahead=%d, want %v ahead=%d",
				c.in, got.nums, got.ahead, c.nums, c.ahead)
		}
	}
}

// 同一 tag 的 describe 版本必须比该 tag 新；反过来则旧。
// 这是「镜像构建后能否继续热更新」的判据。
func TestDescribeSuffixIsNewerThanTag(t *testing.T) {
	tag, _ := parseBuildVersion("v3.14.2")
	ahead, _ := parseBuildVersion("v3.14.2-6-g5018cf2")
	if ahead.compare(tag) != 1 {
		t.Errorf("v3.14.2-6-g5018cf2 应比 v3.14.2 新，compare = %d", ahead.compare(tag))
	}
	if tag.compare(ahead) != -1 {
		t.Errorf("v3.14.2 应比 v3.14.2-6-g5018cf2 旧，compare = %d", tag.compare(ahead))
	}
	if tag.compare(tag) != 0 {
		t.Error("同一版本 compare 应为 0")
	}
	// 提交距离不同：6 比 3 新
	more, _ := parseBuildVersion("v3.14.2-6-gabcdef1")
	less, _ := parseBuildVersion("v3.14.2-3-gabcdef1")
	if more.compare(less) != 1 {
		t.Error("同 tag 下提交距离大的应更新")
	}
}

// 端到端：跑着 describe 版本时，指向其基础 tag 的 Release 不应被判成可升级
// （否则面板会提示「有新版」并让人降级）；指向更新 tag 的应当可升级。
func TestUpdateAvailableWithDescribeVersion(t *testing.T) {
	previous := versionpkg.Version
	t.Cleanup(func() { versionpkg.Version = previous })

	versionpkg.Version = "v3.14.2-6-g5018cf2"
	for tag, want := range map[string]bool{
		"v3.14.2":            false, // 基础 tag，比当前旧
		"v3.14.1":            false,
		"v3.14.3":            true,
		"v3.15.0":            true,
		"v3.14.2-9-gdeadbee": true, // 同 tag 但更靠后
	} {
		if got := (Release{Tag: tag}).UpdateAvailable(); got != want {
			t.Errorf("update from %s to %s = %v, want %v",
				versionpkg.Version, tag, got, want)
		}
	}

	// 跑着正式 tag 时，describe 版本比它新 → 可升级（用于从 tag 升到后续构建）
	versionpkg.Version = "v3.14.2"
	if !(Release{Tag: "v3.14.2-6-g5018cf2"}).UpdateAvailable() {
		t.Error("v3.14.2 -> v3.14.2-6-g5018cf2 应判定为可升级")
	}
	if (Release{Tag: "v3.14.2"}).UpdateAvailable() {
		t.Error("v3.14.2 -> v3.14.2 不应判定为可升级")
	}
}
