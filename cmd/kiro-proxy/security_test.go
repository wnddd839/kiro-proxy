package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
)

func TestGuardListen(t *testing.T) {
	cases := []struct {
		name     string
		listen   string
		hasKeys  bool
		allow    bool
		wantErr  bool
		wantWarn bool
	}{
		{"loopback no keys", "127.0.0.1:8787", false, false, false, false},
		{"wildcard with keys", "0.0.0.0:8787", true, false, false, false},
		{"wildcard no keys", "0.0.0.0:8787", false, false, true, false},
		{"wildcard no keys overridden", "0.0.0.0:8787", false, true, false, true},
		{"nonloopback no keys", "192.168.1.5:8787", false, false, true, false},
		{"empty host no keys", ":8787", false, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf strings.Builder
			log := slog.New(slog.NewTextHandler(&buf, nil))
			cfg := config.Default()
			cfg.Listen = c.listen
			err := guardListen(cfg, c.hasKeys, c.allow, log)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got := strings.Contains(buf.String(), "SECURITY"); got != c.wantWarn {
				t.Fatalf("warn = %q (contains SECURITY=%v), want %v", buf.String(), got, c.wantWarn)
			}
		})
	}
}

func TestNewHTTPServerSetsReadTimeouts(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:8787"
	srv := newHTTPServer(cfg, http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout not set: %v", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout not set: %v", srv.ReadTimeout)
	}
	if srv.ReadTimeout < srv.ReadHeaderTimeout {
		t.Fatalf("ReadTimeout %v should cover ReadHeaderTimeout %v", srv.ReadTimeout, srv.ReadHeaderTimeout)
	}
	// 显式空闲超时，避免 keep-alive 连接被 ReadTimeout 兜底断开。
	if srv.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %v, want 60s", srv.IdleTimeout)
	}
	if srv.IdleTimeout <= srv.ReadTimeout {
		t.Fatalf("IdleTimeout %v should exceed ReadTimeout %v so keep-alive reuse is not cut short", srv.IdleTimeout, srv.ReadTimeout)
	}
	if srv.Addr != cfg.Listen {
		t.Fatalf("addr = %q, want %q", srv.Addr, cfg.Listen)
	}
}

// 保证配置里的超时值被尊重。
func TestHTTPServerReadTimeoutConfigurable(t *testing.T) {
	cfg := config.Default()
	cfg.ServerReadTimeout = config.Duration(90 * time.Second)
	srv := newHTTPServer(cfg, http.NotFoundHandler())
	if srv.ReadTimeout != 90*time.Second {
		t.Fatalf("ReadTimeout = %v, want 90s", srv.ReadTimeout)
	}
	_ = io.Discard
}

func TestKeysConfiguredIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	// 旧版明文 key：keysConfigured 不能触发迁移改写。
	legacy := `{"keys":[{"id":"k1","name":"legacy","secret":"plaintext-secret-value-1234"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if !keysConfigured(path) {
		t.Fatal("legacy keys file should count as configured")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != legacy {
		t.Fatalf("keysConfigured rewrote the key file: %s", after)
	}
	// 缺失 / 空库 -> false。
	if keysConfigured(filepath.Join(t.TempDir(), "nope.json")) {
		t.Fatal("missing file should not count as configured")
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte(`{"keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if keysConfigured(empty) {
		t.Fatal("empty keys file should not count as configured")
	}
}

func TestKeysConfiguredRequiresUsableCredential(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		json string
		want bool
	}{
		{"valid hash", `{"keys":[{"id":"k","secret_sha256":"` + hash + `"}]}`, true},
		{"plaintext secret", `{"keys":[{"id":"k","secret":"s3cret-value"}]}`, true},
		{"empty record", `{"keys":[{"id":"k"}]}`, false},
		{"blank secret", `{"keys":[{"id":"k","secret":"","secret_sha256":""}]}`, false},
		{"short hash", `{"keys":[{"id":"k","secret_sha256":"abcd"}]}`, false},
		{"non-hex hash", `{"keys":[{"id":"k","secret_sha256":"` + strings.Repeat("zz", 32) + `"}]}`, false},
		{"disabled", `{"keys":[{"id":"k","secret_sha256":"` + hash + `","disabled":true}]}`, false},
		{"expired", `{"keys":[{"id":"k","secret_sha256":"` + hash + `","expires_at":"2000-01-01T00:00:00Z"}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "keys.json")
			if err := os.WriteFile(p, []byte(c.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := keysConfigured(p); got != c.want {
				t.Fatalf("keysConfigured = %v, want %v", got, c.want)
			}
		})
	}
}

func TestHasDownstreamKeysIgnoresBlank(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want bool
	}{
		{"none", nil, false},
		{"blank", []string{""}, false},
		{"spaces", []string{"   "}, false},
		{"blank plus real", []string{"", "real-key"}, true},
		{"real", []string{"real-key"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.APIKeys = c.keys
			cfg.KeysFile = "" // 不落盘 key 库
			if got := hasDownstreamKeys(cfg); got != c.want {
				t.Fatalf("hasDownstreamKeys(%q) = %v, want %v", c.keys, got, c.want)
			}
		})
	}
}
