// ═══ 更新日志 ═══
// 2026-10-13：初版——锁住 Go 面板后端与 Python 版 panel/app.py 的**逐字兼容性**。
//
// 为什么这组测试必须存在：Go 版要接手已在跑的部署，那些部署里已经有
// credentials.json（存着 apr1 或 pbkdf2 哈希）和浏览器里的登录 cookie。
// 任何一处编码差异都会让所有用户被登出、老密码全部失效，而且症状极难定位
// （「密码明明对却登不进」）。参照向量由 Python 侧算法现场生成并固定在此。
package panel

import (
	"strings"
	"testing"
	"time"
)

// apr1 参照向量：由 Python 版 apr1_crypt 生成（见测试说明）。
//
// 这些值也等价于 `openssl passwd -apr1 -salt <salt> <password>`，
// 所以它们同时锁住了「与 Apache/nginx 兼容」这一条。
func TestApr1MatchesPythonVectors(t *testing.T) {
	cases := []struct {
		password string
		salt     string
		want     string
	}{
		{"hello", "abcdefgh", "$apr1$abcdefgh$cy3DSYyOwfhsyu7QOMQzd."},
		{"P@ssw0rd!", "XY12zw34", "$apr1$XY12zw34$gzD0QWyvNgQt5UC9gzoln."},
		{"a", "zz", "$apr1$zz$b4auMQtsu0x0qvrSlYQZ4."},
		{strings.Repeat("a", 100), "12345678", "$apr1$12345678$XUgawe9rI.Q.pC4iat45x/"},
	}
	for _, tc := range cases {
		got := apr1Crypt(tc.password, tc.salt)
		if got != tc.want {
			t.Errorf("apr1Crypt(%q, %q)\n got %q\nwant %q", tc.password, tc.salt, got, tc.want)
		}
	}
}

// TestApr1VerifyRealHashes 用真实哈希串走完整校验路径（不只是重新计算）。
func TestApr1VerifyRealHashes(t *testing.T) {
	stored := "$apr1$abcdefgh$cy3DSYyOwfhsyu7QOMQzd."
	if !VerifyPassword("hello", stored) {
		t.Fatal("正确的口令应通过 apr1 校验")
	}
	if VerifyPassword("wrong", stored) {
		t.Fatal("错误口令不得通过")
	}
	if VerifyPassword("hello", "$apr1$bad") {
		t.Fatal("畸形哈希不得通过")
	}
}

// TestPBKDF2StoredFormatMatchesPython PBKDF2 串的编码形态必须与 Python 相同。
//
// 关键点：用的是**标准 base64（带 = padding）**，不是 URL-safe、也不是 Raw。
// 用错编码会生成「自己能验、Python 验不过」的哈希——反向也一样。
func TestPBKDF2StoredFormatMatchesPython(t *testing.T) {
	const stored = "pbkdf2_sha256$200000$AAECAwQFBgcICQoLDA0ODw==$f2A+BZ5F0J+/XtK6mfeaoGhmyQTtH5XVbdPAY7HnBP4="
	if !VerifyPassword("secret", stored) {
		t.Fatal("Python 生成的 pbkdf2 串必须能被 Go 校验")
	}
	if VerifyPassword("Secret", stored) {
		t.Fatal("口令大小写敏感")
	}
	// 反向：Go 生成的串结构必须与 Python 的模板一致。
	generated, err := HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(generated, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" || parts[1] != "200000" {
		t.Fatalf("串结构不对：%q", generated)
	}
	if !strings.HasSuffix(parts[2], "==") || !strings.HasSuffix(parts[3], "=") {
		t.Fatalf("必须是带 padding 的标准 base64：%q", generated)
	}
	if !VerifyPassword("hunter2", generated) {
		t.Fatal("自己生成的串必须能自验")
	}
}

// TestSessionTokenFormatMatchesPython 会话令牌必须与 Python 版逐字节兼容。
//
// 格式：base64url(payload) + "." + base64url(HMAC-SHA256(key, body))，
// payload 是**无空格**的 JSON，字段序 u → e → n。
// 有空格或多一个字段，签名就对不上——现有用户的 cookie 会全部失效。
func TestSessionTokenFormatMatchesPython(t *testing.T) {
	creds := Credentials{
		"username":   "wbadmin",
		"password":   "x",
		"sessionKey": strings.Repeat("ab", 32), // 64 hex
	}
	// 固定 nonce，断言可复现。
	token, err := IssueSession(creds, "0123456789abcdef01234567", SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	body, sig, found := strings.Cut(token, ".")
	if !found {
		t.Fatalf("令牌必须含分隔点：%q", token)
	}
	// 无 padding（Python 用 rstrip("=") / RawURLEncoding）。
	if strings.Contains(body, "=") || strings.Contains(sig, "=") {
		t.Fatalf("base64url 不得带 padding：%q", token)
	}
	// 解出 payload 检查字段与顺序。
	decoded := decodeRawURL(t, body)
	if !strings.HasPrefix(decoded, `{"u":"wbadmin","e":`) {
		t.Fatalf("payload 形态/字段序不对：%s", decoded)
	}
	if !strings.Contains(decoded, `,"n":"0123456789abcdef01234567"}`) {
		t.Fatalf("payload 字段序应为 u,e,n：%s", decoded)
	}
	if strings.Contains(decoded, ": ") || strings.Contains(decoded, ", ") {
		t.Fatalf("JSON 不得含空格：%s", decoded)
	}
	// 自己签的必须能自验。
	if got := ReadSession(creds, token); got == nil {
		t.Fatal("自签令牌必须能通过校验")
	}
}

// TestReadSessionRejectsTampering 篡改任一字节都必须失败。
func TestReadSessionRejectsTampering(t *testing.T) {
	creds := Credentials{
		"username":   "wbadmin",
		"password":   "x",
		"sessionKey": strings.Repeat("ab", 32),
	}
	token, err := IssueSession(creds, "", SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if ReadSession(creds, "") != nil {
		t.Fatal("空令牌不得通过")
	}
	if ReadSession(creds, "garbage") != nil {
		t.Fatal("无分隔点的令牌不得通过")
	}
	body, sig, _ := strings.Cut(token, ".")
	if ReadSession(creds, body+"."+sig+"x") != nil {
		t.Fatal("签名被追加字符后不得通过")
	}
	// 换一个 sessionKey（模拟换密钥）后旧令牌必须失效。
	other := Credentials{"username": "wbadmin", "password": "x", "sessionKey": strings.Repeat("cd", 32)}
	if ReadSession(other, token) != nil {
		t.Fatal("换了会话密钥后旧令牌必须失效")
	}
	// 用户名不匹配必须失效。
	renamed := Credentials{"username": "someone", "password": "x", "sessionKey": strings.Repeat("ab", 32)}
	if ReadSession(renamed, token) != nil {
		t.Fatal("用户名不匹配必须失效")
	}
}

// TestReadSessionRejectsExpired 过期令牌必须失效。
func TestReadSessionRejectsExpired(t *testing.T) {
	creds := Credentials{
		"username":   "wbadmin",
		"password":   "x",
		"sessionKey": strings.Repeat("ab", 32),
	}
	token, err := IssueSession(creds, "", -time.Hour) // 已过期
	if err != nil {
		t.Fatal(err)
	}
	if ReadSession(creds, token) != nil {
		t.Fatal("过期令牌不得通过")
	}
}

// TestRevokedSessionStaysRevoked 登出后令牌必须失效，且落盘后仍失效。
func TestRevokedSessionStaysRevoked(t *testing.T) {
	dir := t.TempDir()
	store := &CredentialStore{Path: dir + "/credentials.json"}
	creds := Credentials{
		"username":   "wbadmin",
		"password":   "x",
		"sessionKey": strings.Repeat("ab", 32),
	}
	const nonce = "0123456789abcdef01234567"
	token, err := IssueSession(creds, nonce, SessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if ReadSession(creds, token) == nil {
		t.Fatal("登出前应有效")
	}
	if err := store.RevokeSession(creds, nonce); err != nil {
		t.Fatal(err)
	}
	if ReadSession(creds, token) != nil {
		t.Fatal("登出后必须失效")
	}
	// 重新加载（模拟重启）后仍失效——撤销记录必须已落盘。
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ReadSession(reloaded, token) != nil {
		t.Fatal("重启后撤销状态必须保持")
	}
}

// TestCredentialsPreservesUnknownFields 保存时不得丢掉未知字段。
//
// 运维文件被工具悄悄改写是危险的：用户可能手动加了备注字段，或未来版本加了
// 新键，用 struct 解析再序列化会把它们抹掉。
func TestCredentialsPreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/credentials.json"
	store := &CredentialStore{Path: path}
	creds := Credentials{
		"username":    "wbadmin",
		"password":    "x",
		"sessionKey":  strings.Repeat("ab", 32),
		"customNote":  "用户手动加的",
		"futureField": float64(42),
	}
	if err := store.Save(creds); err != nil {
		t.Fatal(err)
	}
	back, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if back["customNote"] != "用户手动加的" {
		t.Fatalf("未知字段丢失：%v", back)
	}
	if back["futureField"] != float64(42) {
		t.Fatalf("未知数值字段丢失：%v", back)
	}
}

// TestLoadRejectsInvalidCredentials 内容不合法的凭证文件必须报错，不能静默重造。
//
// 静默重造会让用户「突然被要求设置新密码」，且原文件被覆盖——原本可恢复的
// 情况（比如手工编辑出错）就变成不可恢复了。
func TestLoadRejectsInvalidCredentials(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/credentials.json"
	store := &CredentialStore{Path: path}

	write := func(content string) {
		if err := writeFile(path, content); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"username":"a"}`)
	if _, err := store.Load(); err == nil {
		t.Fatal("缺 password 必须报错")
	}
	write(`{"username":"a","password":"b","sessionKey":"nothex"}`)
	if _, err := store.Load(); err == nil {
		t.Fatal("sessionKey 非法必须报错")
	}
	write(`{"username":"a","password":"b","sessionKey":"` + strings.Repeat("ab", 32) +
		`","revokedSessions":"oops"}`)
	if _, err := store.Load(); err == nil {
		t.Fatal("revokedSessions 类型不对必须报错")
	}
}

// TestLoadCreatesInitialCredentials 首次运行要生成可用凭证与提示文件。
func TestLoadCreatesInitialCredentials(t *testing.T) {
	dir := t.TempDir()
	store := &CredentialStore{Path: dir + "/credentials.json"}
	creds, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username() != "wbadmin" {
		t.Fatalf("初始用户名应为 wbadmin，实际 %q", creds.Username())
	}
	if _, err := creds.SessionKey(); err != nil {
		t.Fatalf("初始凭证必须带合法 sessionKey：%v", err)
	}
	hint := readFile(dir + "/initial-password.txt")
	if !strings.Contains(hint, "username: wbadmin") || !strings.Contains(hint, "password: ") {
		t.Fatalf("初始口令提示文件内容不对：%q", hint)
	}
}
