// ═══ 更新日志 ═══
// 2026-10-02：第二轮体检确认缺陷的回归测试（密钥名分隔符、迁移长度、List 吞错）。
package apikeys

import (
	"strings"
	"testing"
)

// TestKeyNameRejectsLineSeparators U+2028/U+2029 必须被拒。
//
// 它们是行/段分隔符（Zl/Zp），unicode.IsControl 只覆盖 Cc 拦不住。带这类字符的
// 密钥名能通过校验，但 requestlog 会拒绝该名字的记录，于是该密钥的每一次请求都
// 静默不进「请求明细」。从网页复制粘贴的文本常带这些字符，属正常运维误操作。
//
// 2026-10-02 第二轮体检发现。
func TestKeyNameRejectsLineSeparators(t *testing.T) {
	for _, name := range []string{"正常名字 ", " 前缀", "a b"} {
		if validLabel(name, "") {
			t.Errorf("validLabel(%q) = true，行分隔符应被拒绝", name)
		}
	}
	// 正常名字与备注仍须接受。
	if !validLabel("正常名字", "正常备注") {
		t.Fatal("正常名字被误拒")
	}
}

// TestOversizedConfiguredKeyIsRejected 超过 MaxKeyLength 的配置密钥必须明确报错。
//
// 此前它被迁移成一条「已启用」记录，但 Lookup 在比较前就按长度拒绝，
// 于是这把密钥永远无法认证，管理台却显示它存在，运维查不出原因。
//
// 2026-10-02 第二轮体检发现。
func TestOversizedConfiguredKeyIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/keys.json"
	oversized := strings.Repeat("k", MaxKeyLength+1)
	if _, err := Open(path, oversized); err == nil {
		t.Fatal("超长配置密钥应被拒绝，而不是迁移成一条永远无法认证的记录")
	}
	// 恰好等于上限仍须接受。
	if _, err := Open(path, strings.Repeat("k", MaxKeyLength)); err != nil {
		t.Fatalf("恰好 %d 字节的密钥应被接受: %v", MaxKeyLength, err)
	}
}
