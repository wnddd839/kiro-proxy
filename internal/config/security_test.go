package config

import (
	"testing"
	"time"
)

func TestReachesBeyondLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8787", false},
		{"localhost:8787", false},
		{"[::1]:8787", false},
		{"127.0.0.1", false},
		{"0.0.0.0:8787", true},
		{":8787", true},
		{"[::]:8787", true},
		{"", true},
		{"192.168.1.5:8787", true},
		{"api.example.com:8787", true},
	}
	for _, c := range cases {
		if got := ReachesBeyondLoopback(c.listen); got != c.want {
			t.Errorf("ReachesBeyondLoopback(%q) = %v, want %v", c.listen, got, c.want)
		}
	}
}

func TestUnsafeListenRequiresKeys(t *testing.T) {
	cases := []struct {
		listen  string
		hasKeys bool
		want    bool
	}{
		{"127.0.0.1:8787", false, false}, // loopback is fine without keys
		{"0.0.0.0:8787", true, false},    // keys configured: allowed
		{"0.0.0.0:8787", false, true},    // wildcard, no keys: unsafe
		{"192.168.1.5:8787", false, true},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Listen = c.listen
		if got := cfg.UnsafeListen(c.hasKeys); got != c.want {
			t.Errorf("UnsafeListen(%q, keys=%v) = %v, want %v", c.listen, c.hasKeys, got, c.want)
		}
	}
}

func TestDebugRequestsDefaultsAndValidation(t *testing.T) {
	c := Default()
	if c.DebugRequestsEnabled() {
		t.Fatal("debug requests must default off")
	}
	if c.DebugRequestsCap() != DefaultDebugRequestsMaxBytes || c.DebugRequestsTTL() != DefaultDebugRequestsRetention {
		t.Fatalf("defaults: cap %d ttl %v", c.DebugRequestsCap(), c.DebugRequestsTTL())
	}
	c.DebugRequestsDir = t.TempDir()
	c.DebugRequestsMaxBytes = 123
	c.DebugRequestsRetention = Duration(2 * time.Hour)
	if c.DebugRequestsCap() != 123 || c.DebugRequestsTTL() != 2*time.Hour || !c.DebugRequestsEnabled() {
		t.Fatalf("overrides: cap %d ttl %v enabled %v", c.DebugRequestsCap(), c.DebugRequestsTTL(), c.DebugRequestsEnabled())
	}

	for _, bad := range []struct {
		name string
		cap  int64
		ttl  time.Duration
	}{
		{"cap", -1, 0},
		{"retention", 0, -1},
	} {
		cfg := Default()
		cfg.DebugRequestsDir = t.TempDir()
		cfg.DebugRequestsMaxBytes, cfg.DebugRequestsRetention = bad.cap, Duration(bad.ttl)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want validation error", bad.name)
		}
	}
}

// 空的 api key 会让 ConstantTimeCompare 把"没带 key"的请求判成通过；必须拒绝。
func TestValidateRejectsBlankAPIKeys(t *testing.T) {
	for _, keys := range [][]string{{""}, {"   "}, {"\t"}, {"ok", ""}, {"ok", "  "}} {
		c := Default()
		c.APIKeys = keys
		if err := c.Validate(); err == nil {
			t.Errorf("APIKeys=%q: want validation error", keys)
		}
	}
	for _, keys := range [][]string{nil, {}, {"chosen-key"}, {"key-a", "key-b"}} {
		c := Default()
		c.APIKeys = keys
		if err := c.Validate(); err != nil {
			t.Errorf("APIKeys=%q: unexpected error %v", keys, err)
		}
	}
}
