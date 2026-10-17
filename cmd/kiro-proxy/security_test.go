package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wnddd839/kiro.cache/internal/config"
)

// 启动守卫：监听可能被本机之外访问、又没有可用的下游 key 时拒绝启动；显式覆盖时放行并醒目告警。
// key 由 api_keys（忽略空白）或 keys_file 里可用的记录提供；守卫只读 key 库，不触发明文迁移改写。
func TestStartupGuardsExposedListener(t *testing.T) {
	const missing = "\x00missing"
	hash := strings.Repeat("ab", 32)
	cases := []struct {
		name   string
		listen string
		api    []string
		file   string // keys_file 内容；空 = 不配置 keys_file；missing = 配了但文件不存在
		allow  bool
		want   string // ok | refuse | warn
	}{
		{name: "loopback ipv4", listen: "127.0.0.1:8787", want: "ok"},
		{name: "localhost", listen: "localhost:8787", want: "ok"},
		{name: "localhost subdomain", listen: "app.localhost:8787", want: "ok"},
		{name: "loopback ipv6", listen: "[::1]:8787", want: "ok"},
		{name: "loopback without port", listen: "127.0.0.1", want: "ok"},
		{name: "wildcard", listen: "0.0.0.0:8787", want: "refuse"},
		{name: "empty host", listen: ":8787", want: "refuse"},
		{name: "ipv6 wildcard", listen: "[::]:8787", want: "refuse"},
		{name: "lan address", listen: "192.168.1.5:8787", want: "refuse"},
		{name: "domain name", listen: "api.example.com:8787", want: "refuse"},
		{name: "api key", listen: "0.0.0.0:8787", api: []string{"real-key"}, want: "ok"},
		{name: "blank api keys", listen: "0.0.0.0:8787", api: []string{"", "   "}, want: "refuse"},
		{name: "blank plus real api key", listen: "0.0.0.0:8787", api: []string{"", "real-key"}, want: "ok"},
		{name: "keys file missing", listen: "0.0.0.0:8787", file: missing, want: "refuse"},
		{name: "keys file empty", listen: "0.0.0.0:8787", file: `{"keys":[]}`, want: "refuse"},
		{name: "keys file emptied but auth required", listen: "0.0.0.0:8787", file: `{"keys":[],"require_auth":true}`, want: "ok"},
		{name: "hashed key", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret_sha256":"` + hash + `"}]}`, want: "ok"},
		{name: "legacy plaintext key", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k1","name":"legacy","secret":"plaintext-secret-value-1234"}]}`, want: "ok"},
		{name: "only disabled key", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret_sha256":"` + hash + `","disabled":true}]}`, want: "refuse"},
		{name: "only expired key", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret_sha256":"` + hash + `","expires_at":"2000-01-01T00:00:00Z"}]}`, want: "refuse"},
		{name: "record without credential", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret":"","secret_sha256":""}]}`, want: "refuse"},
		{name: "short hash", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret_sha256":"abcd"}]}`, want: "refuse"},
		{name: "non-hex hash", listen: "0.0.0.0:8787", file: `{"keys":[{"id":"k","secret_sha256":"` + strings.Repeat("zz", 32) + `"}]}`, want: "refuse"},
		{name: "explicit override", listen: "0.0.0.0:8787", allow: true, want: "warn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Listen, cfg.APIKeys, cfg.KeysFile = c.listen, c.api, ""
			if c.file != "" {
				cfg.KeysFile = filepath.Join(t.TempDir(), "keys.json")
				if c.file != missing {
					if err := os.WriteFile(cfg.KeysFile, []byte(c.file), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			var logs strings.Builder
			err := guardListen(cfg, hasDownstreamKeys(cfg), c.allow, slog.New(slog.NewTextHandler(&logs, nil)))
			if (err != nil) != (c.want == "refuse") || strings.Contains(logs.String(), "SECURITY") != (c.want == "warn") {
				t.Fatalf("want %s: err=%v logs=%q", c.want, err, logs.String())
			}
			if c.file != "" && c.file != missing {
				if after, _ := os.ReadFile(cfg.KeysFile); string(after) != c.file {
					t.Fatalf("startup guard rewrote the key file: %s", after)
				}
			}
		})
	}
}

// 下游读超时可配置，请求头超时不超过它；空闲超时显式 60s，免得复用的连接被读超时兜底掐断。
func TestHTTPServerTimeouts(t *testing.T) {
	for _, c := range []struct {
		read               config.Duration
		wantRead, wantHead time.Duration
	}{
		{0, 30 * time.Second, 10 * time.Second},
		{config.Duration(90 * time.Second), 90 * time.Second, 10 * time.Second},
		{config.Duration(5 * time.Second), 5 * time.Second, 5 * time.Second},
	} {
		cfg := config.Default()
		cfg.ServerReadTimeout = c.read
		srv := newHTTPServer(cfg, http.NotFoundHandler())
		if srv.ReadTimeout != c.wantRead || srv.ReadHeaderTimeout != c.wantHead || srv.IdleTimeout != 60*time.Second || srv.Addr != cfg.Listen {
			t.Errorf("read=%v: got read=%v header=%v idle=%v addr=%q", time.Duration(c.read), srv.ReadTimeout, srv.ReadHeaderTimeout, srv.IdleTimeout, srv.Addr)
		}
	}
}
