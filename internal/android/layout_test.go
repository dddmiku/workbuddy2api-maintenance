// ═══ 更新日志 ═══
// 2026-10-13：初版——锁住安卓端布局与辅助程序解析的可观测行为。
package android

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutEnsureCreatesAllDirectories(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(root, filepath.Join(root, "lib"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{layout.ConfigDir, layout.Auths, layout.AuthTrash,
		layout.DataDir, layout.PanelData, layout.LogDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("目录必须建出来：%s（%v）", dir, err)
		}
	}
	// 幂等：再调一次不该报错，也不该动已有内容。
	marker := filepath.Join(layout.Auths, "keep.json")
	if err := os.WriteFile(marker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("Ensure 必须幂等：%v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("Ensure 不得动已有文件")
	}
}

func TestEnsureConfigNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	layout := NewLayout(root, filepath.Join(root, "lib"))
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	created, err := layout.EnsureConfig()
	if err != nil || !created {
		t.Fatalf("首次应创建配置：created=%v err=%v", created, err)
	}
	// 用户改过的配置绝不能被覆盖——「重启一次设置就没了」会让人再也不敢改配置。
	custom := "{\n  \"listen\": \"127.0.0.1:9999\"\n}\n"
	if err := os.WriteFile(layout.Config, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	created, err = layout.EnsureConfig()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("已存在配置时不得重写")
	}
	raw, err := os.ReadFile(layout.Config)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != custom {
		t.Fatalf("配置被改动了：%s", raw)
	}
}

func TestDefaultConfigBindsLoopbackOnly(t *testing.T) {
	// 手机常连公共 Wi-Fi：监听 0.0.0.0 意味着同一个网络里的人可以直接用你的账号池
	// （而且 api_key 默认是空的）。这一条锁住「只绑回环」这个安全默认值。
	config := DefaultConfig()
	if !strings.Contains(config, `"listen": "127.0.0.1:7863"`) {
		t.Fatalf("必须只绑回环：%s", config)
	}
	if strings.Contains(config, "0.0.0.0") {
		t.Fatalf("不得监听所有网卡：%s", config)
	}
	// 自动更新在手机上关掉：便携包靠换 APK 更新，而不是让二进制自我替换
	// （在安卓上会撞 SELinux 与应用私有目录的 noexec）。
	if !strings.Contains(config, `"enabled": false`) {
		t.Fatalf("安卓上必须关掉自我热更新：%s", config)
	}
	// 脚本类任务默认关闭：APK 里既没有 scripts/*.py 也没有 Python，开着只会在
	// 每个时点各报一次 WARN（用户看到的是「定时任务一直失败」）。
	for _, key := range []string{"school_enabled", "cat_enabled", "redeem_enabled",
		"lottery_enabled", "makeup_enabled"} {
		if !strings.Contains(config, `"`+key+`": false`) {
			t.Fatalf("脚本类任务 %s 在安卓上必须默认关闭：%s", key, config)
		}
	}
	// Go 实现的任务必须保持开启（它们不依赖 Python）。
	for _, key := range []string{"checkin_enabled", "travel_enabled", "activity_enabled",
		"keepalive_enabled", "trae_check_enabled"} {
		if !strings.Contains(config, `"`+key+`": true`) {
			t.Fatalf("Go 实现的任务 %s 必须默认开启：%s", key, config)
		}
	}
}

func TestLibNameMatchesRuntimeLookup(t *testing.T) {
	// 打包侧（android/build.py）与运行时（internal/android）共用这一份命名规则。
	// 分叉的表现是「按钮点了报『缺少辅助程序』」——只在真机上暴露，事后极难定位。
	cases := map[string]string{
		"gateway":     "libwb2gateway.so",
		"login":       "libwb2login.so",
		"credit":      "libwb2credit.so",
		"fission_bin": "libwb2fission_bin.so",
	}
	for helper, want := range cases {
		if got := LibName(helper); got != want {
			t.Fatalf("LibName(%q) = %q，期望 %q", helper, got, want)
		}
	}
	// HelperNames 必须与 build.py 的 HELPERS 一致（外加网关本体不在这个列表里，
	// 它由 AppPaths.gatewayBinary 单独解析）。
	want := []string{"login", "credit", "fission_bin"}
	if len(HelperNames) != len(want) {
		t.Fatalf("HelperNames 应为 %v，实际 %v", want, HelperNames)
	}
	for index, name := range want {
		if HelperNames[index] != name {
			t.Fatalf("HelperNames[%d] 应为 %q，实际 %q", index, name, HelperNames[index])
		}
	}
}

func TestResolveHelpers(t *testing.T) {
	dir := t.TempDir()
	t.Run("全在", func(t *testing.T) {
		for _, name := range HelperNames {
			if err := os.WriteFile(filepath.Join(dir, LibName(name)), []byte("x"), 0755); err != nil {
				t.Fatal(err)
			}
		}
		resolved, err := ResolveHelpers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved) != len(HelperNames) {
			t.Fatalf("应解析出全部辅助程序：%v", resolved)
		}
	})
	t.Run("缺一个要说清楚缺哪个", func(t *testing.T) {
		partial := t.TempDir()
		if err := os.WriteFile(filepath.Join(partial, LibName("login")), []byte("x"), 0755); err != nil {
			t.Fatal(err)
		}
		_, err := ResolveHelpers(partial)
		if err == nil {
			t.Fatal("缺文件必须报错")
		}
		// 「按钮点了没反应」是最难定位的一类故障；错误信息必须直接指向缺哪个。
		if !strings.Contains(err.Error(), "libwb2credit.so") || !strings.Contains(err.Error(), "libwb2fission_bin.so") {
			t.Fatalf("必须列出缺失的文件名：%v", err)
		}
		if strings.Contains(err.Error(), "libwb2login.so") {
			t.Fatalf("已存在的文件不该被列成缺失：%v", err)
		}
	})
	t.Run("空目录参数", func(t *testing.T) {
		if _, err := ResolveHelpers(""); err == nil {
			t.Fatal("空 nativeLibraryDir 必须报错（打包配置问题）")
		}
	})
}
