// ═══ 更新日志 ═══
// 2026-09-24：复现连接串报错泄露凭据、REST 路径和特殊字符密码导致静默降级的问题。
package redisstore

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestMalformedConnectionURLDoesNotLogCredentials(t *testing.T) {
	const credential = "synthetic-audit-password"
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	if _, ok := New("rediss://default:"+credential+"@bad host:6379", "").(Noop); !ok {
		t.Fatal("malformed endpoint must fall back without dialing")
	}
	if strings.Contains(output.String(), credential) {
		t.Fatal("connection parsing failure logged the configured password")
	}
	if output.Len() == 0 {
		t.Fatal("configuration failure must still produce a diagnostic")
	}
}

func TestNormalizeConnectionURLPreservesPasswordAndHost(t *testing.T) {
	const password = "synthetic:/@?#%+password"
	for _, endpoint := range []string{
		"foo.upstash.io",
		"https://foo.upstash.io/path/to/rest?query=1#section",
		"https://foo.upstash.io/",
	} {
		t.Run(endpoint, func(t *testing.T) {
			options, err := redis.ParseURL(normalizeURL(endpoint, password))
			if err != nil {
				t.Fatal("normalized connection URL is invalid")
			}
			if options.Addr != "foo.upstash.io:6379" || options.Username != "default" || options.Password != password {
				t.Fatal("normalization changed the endpoint or password")
			}
			if options.TLSConfig == nil {
				t.Fatal("REST endpoint normalization must retain TLS")
			}
		})
	}
}
