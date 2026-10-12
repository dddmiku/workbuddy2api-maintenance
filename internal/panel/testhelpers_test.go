package panel

import (
	"encoding/base64"
	"os"
	"testing"
)

// decodeRawURL 解 base64url（无 padding）并断言是合法 UTF-8。
func decodeRawURL(t *testing.T, value string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("base64url 解码失败 %q: %v", value, err)
	}
	return string(raw)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
