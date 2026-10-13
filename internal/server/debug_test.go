package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/usage"
)

// New 即使拿到裸 logger 也必须包上脱敏 handler，防止测试 / 第三方直传时泄漏。
func TestNewWrapsLoggerWithRedaction(t *testing.T) {
	var buf bytes.Buffer
	bare := slog.New(slog.NewTextHandler(&buf, nil))
	p, err := pool.Open(filepath.Join(t.TempDir(), "accounts.json"), pool.Options{Logger: bare})
	if err != nil {
		t.Fatal(err)
	}
	bg, stop := context.WithCancel(context.Background())
	defer stop()
	cfg := config.Default()
	cfg.CacheFile, cfg.PriceSync, cfg.PricesFile = "", 0, ""
	s, err := New(bg, cfg, p, bare, Stores{})
	if err != nil {
		t.Fatal(err)
	}
	s.log.Error("boom", "token", "sk-secret-abcdefghijkl")
	if strings.Contains(buf.String(), "sk-secret-abcdefghijkl") {
		t.Fatalf("server logger leaked secret: %s", buf.String())
	}
}

// 调试快照：落盘内容已脱敏；目录按字节上限淘汰最旧快照、最新的保留；单条超限的快照整条跳过，不挤掉已有快照。
func TestDebugSnapshotsRedactedAndBounded(t *testing.T) {
	ok := func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }

	t.Run("redacted", func(t *testing.T) {
		const secret = "sk-live-ABCDEF0123456789"
		dir := t.TempDir()
		h := newHarness(t, 1, func(c *config.Config) { c.DebugRequestsDir = dir })
		h.up.stream = ok
		body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":"system with %s","messages":[{"role":"user","content":[{"type":"text","text":"hi %s"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"SECRETIMAGEPIXELS"}}]}]}`, secret, secret)
		decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": "redact-trace"}))
		waitRequests(t, h, usage.Query{}, 1)
		files := debugFiles(t, dir)
		if len(files) == 0 {
			t.Fatal("no snapshot written")
		}
		redacted := false
		for name := range files {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{secret, "SECRETIMAGEPIXELS", "arn:aws:codewhisperer"} {
				if strings.Contains(string(raw), leak) {
					t.Fatalf("snapshot %s leaked %q", name, leak)
				}
			}
			redacted = redacted || strings.Contains(string(raw), "[REDACTED]")
		}
		if !redacted {
			t.Fatal("no redaction marker; snapshot may hold raw secrets")
		}
	})

	t.Run("bounded", func(t *testing.T) {
		const capBytes = 12000
		dir := t.TempDir()
		h := newHarness(t, 1, func(c *config.Config) {
			c.DebugRequestsDir = dir
			c.DebugRequestsMaxBytes = capBytes
		})
		h.up.stream = ok
		// 每条快照约 4 KB：能装下，但 6 条必然触发淘汰
		body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"hi"}]}`, strings.Repeat("padding ", 190))
		var newest string
		for i := range 6 {
			before := debugFiles(t, dir)
			decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": fmt.Sprintf("cap-%d", i)}))
			waitRequests(t, h, usage.Query{}, int64(i+1))
			after := debugFiles(t, dir)
			var total int64
			newest = ""
			for name, size := range after {
				total += size
				if _, existed := before[name]; !existed && strings.HasSuffix(name, ".latest.json") {
					newest = name
				}
			}
			if total > capBytes || newest == "" {
				t.Fatalf("write %d: total=%d cap=%d newest=%q files=%v", i, total, capBytes, newest, slices.Sorted(maps.Keys(after)))
			}
		}
		latest := 0
		for name := range debugFiles(t, dir) {
			if strings.HasSuffix(name, ".latest.json") {
				latest++
			}
		}
		if latest == 0 || latest >= 6 {
			t.Fatalf("eviction not exercised: %d latest snapshots kept for 6 sessions", latest)
		}

		before := debugFiles(t, dir)
		big := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"hi"}]}`, strings.Repeat("Z", 40000))
		decodeUsage(t, h.post(t, "/v1/messages", big, map[string]string{"X-Session-Id": "over-cap"}))
		waitRequests(t, h, usage.Query{}, 7)
		after := debugFiles(t, dir)
		if !slices.Equal(slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after))) {
			t.Fatalf("over-cap snapshot written or displaced others: before=%v after=%v", slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
		}
	})
}

// 启动即清理：过期组与残缺组（latest 缺 meta）删掉，成对的新快照保留；
// 共享目录里的无关文件、名字相似的文件、子目录、符号链接都不动。
func TestDebugCleanupOnlyRemovesOwnStaleSnapshots(t *testing.T) {
	dir := t.TempDir()
	expired := writeDebugFile(t, dir, stem('a')+".latest.json", 100, 48*time.Hour)
	writeDebugFile(t, dir, stem('a')+".latest.meta.json", 40, 48*time.Hour)
	orphan := writeDebugFile(t, dir, stem('f')+".latest.json", 100, time.Second)
	keep := []string{
		writeDebugFile(t, dir, stem('b')+"-123.request.json", 100, 2*time.Minute),
		writeDebugFile(t, dir, stem('b')+"-123.request.json.meta.json", 40, 2*time.Minute),
		writeDebugFile(t, dir, stem('c')+".latest.json", 100, time.Second),
		writeDebugFile(t, dir, stem('c')+".latest.meta.json", 40, time.Second),
		writeDebugFile(t, dir, "someone-else-notes.json", 100, 72*time.Hour),
		writeDebugFile(t, dir, "deadbeef.latest.json", 100, 72*time.Hour),
	}
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, sub)
	// 符号链接指向目录外也绝不跟随（Windows 上无权限创建时跳过这一项）
	if link := filepath.Join(dir, stem('e')+".latest.json"); os.Symlink(keep[4], link) == nil {
		keep = append(keep, link)
	}

	newHarness(t, 1, func(c *config.Config) {
		c.DebugRequestsDir = dir
		c.DebugRequestsRetention = config.Duration(time.Hour)
	})
	for _, gone := range []string{expired, orphan} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("stale snapshot survived startup cleanup: %s (%v)", filepath.Base(gone), err)
		}
	}
	for _, path := range keep {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("cleanup removed %s: %v", filepath.Base(path), err)
		}
	}
}

// debugFiles 是目录里普通文件的 名字 → 大小。
func debugFiles(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			out[e.Name()] = info.Size()
		}
	}
	return out
}

// writeDebugFile 造一个固定大小、指定年龄的调试文件。
func writeDebugFile(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return path
}

// stem 造 64 位十六进制快照名（真实快照用 sha256）。
func stem(c byte) string { return strings.Repeat(string(c), 64) }
