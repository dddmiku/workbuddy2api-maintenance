package panelruntime

import "testing"

// TestAdminPrefixOr 锁定面板前缀归一化。
//
// 面板被嵌在网关的子路径下，这个前缀同时用于路由注册、代理剥前缀、以及面板拼
// **公网回调地址**。写死 "/admin" 会让别的部署形态（挂在 /panel、或直接挂在根）
// 下 Trae 授权回调指向一个不存在的位置——2026-10-11 用户指出这点时确实写死了。
func TestAdminPrefixOr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/admin"},
		{"/admin", "/admin"},
		{"panel", "/panel"},   // 补前导斜杠
		{"/panel/", "/panel"}, // 去尾斜杠
		{"  /x/y  ", "/x/y"},  // 去空白
		{"/", "/admin"},       // 只有斜杠 → 默认（不能拼出 "//trae/callback"）
		{"/admin/", "/admin"},
	}
	for _, tc := range cases {
		if got := adminPrefixOr(tc.in); got != tc.want {
			t.Errorf("adminPrefixOr(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
