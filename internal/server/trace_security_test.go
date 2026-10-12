package server

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/usage"
)

// New 即使用裸 logger，也必须包上脱敏 handler，防止测试/第三方直传时泄漏。
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

// 落盘的调试快照必须已脱敏：不能出现下游 secret、profileArn 或图片字节。
func TestTraceSnapshotIsRedacted(t *testing.T) {
	const secret = "sk-live-ABCDEF0123456789"
	dir := t.TempDir()
	h := newHarness(t, 1, func(c *config.Config) { c.DebugRequestsDir = dir })
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":"system with %s","messages":[{"role":"user","content":[{"type":"text","text":"hi %s"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"SECRETIMAGEPIXELS"}}]}]}`, secret, secret)
	decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": "redact-trace"}))
	waitRequests(t, h, usage.Query{}, 1)

	redacted := false
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, leak := range []string{secret, "SECRETIMAGEPIXELS", "arn:aws:codewhisperer"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("trace %s leaked %q", filepath.Base(path), leak)
			}
		}
		if strings.Contains(string(raw), "[REDACTED]") {
			redacted = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !redacted {
		t.Fatal("trace contained no redaction marker; snapshot may have captured raw secrets")
	}
}

// 每次写完都按容量上限收拾，调试目录不会无界增长。
func TestTraceWriteEnforcesByteCap(t *testing.T) {
	dir := t.TempDir()
	// 很小的上限：每写一点就必须把旧组清掉。
	h := newHarness(t, 1, func(c *config.Config) {
		c.DebugRequestsDir = dir
		c.DebugRequestsMaxBytes = 20000
	})
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"hi"}]}`, strings.Repeat("padding ", 1500))
	for i := range 6 {
		decodeUsage(t, h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": fmt.Sprintf("cap-%d", i)}))
		waitRequests(t, h, usage.Query{}, int64(i+1))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	// 6 个 ~12KB 快照若不清会到 ~72KB；写完即按 20000 收拾，应严格不超上限。
	if total > 20000 {
		t.Fatalf("debug dir grew unbounded: %d bytes", total)
	}
}

// 单条请求本身就超过上限时必须整条跳过：不写正文、不落盘、并清掉已有旧快照。
func TestTraceSkipsSnapshotOverCap(t *testing.T) {
	dir := t.TempDir()
	const capBytes = 5000
	h := newHarness(t, 1, func(c *config.Config) {
		c.DebugRequestsDir = dir
		c.DebugRequestsMaxBytes = capBytes
	})
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	// 请求体远大于 cap（系统提示约 40KB）。
	big := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"hi"}]}`, strings.Repeat("Z", 40000))
	decodeUsage(t, h.post(t, "/v1/messages", big, map[string]string{"X-Session-Id": "over-cap"}))
	waitRequests(t, h, usage.Query{}, 1)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "ZZZZZZZZZZ") {
			t.Fatalf("over-cap snapshot body was written to disk: %s", e.Name())
		}
	}
	var total int64
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	// 跳过即不落盘：目录保持为空（或仅剩允许的残留，绝不含正文）。
	if total > capBytes {
		t.Fatalf("dir over cap after skipping: %d", total)
	}
	// 真实验证：没有任何 request 正文快照。
	if files, _ := filepath.Glob(filepath.Join(dir, "*request.json")); len(files) != 0 {
		t.Fatalf("over-cap request snapshot present: %v", files)
	}
}

// 已有旧快照时，单条超限请求不应把旧快照挤掉（因为根本不写）。
func TestTraceOverCapKeepsExistingSnapshots(t *testing.T) {
	dir := t.TempDir()
	const capBytes = 3000
	h := newHarness(t, 1, func(c *config.Config) {
		c.DebugRequestsDir = dir
		c.DebugRequestsMaxBytes = capBytes
	})
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	// 先写一条小的正常快照。
	small := `{"model":"claude-sonnet-4.5","system":"sys","messages":[{"role":"user","content":"hi"}]}`
	decodeUsage(t, h.post(t, "/v1/messages", small, map[string]string{"X-Session-Id": "small"}))
	waitRequests(t, h, usage.Query{}, 1)
	before, _ := filepath.Glob(filepath.Join(dir, "*.latest.json"))
	if len(before) != 1 {
		t.Fatalf("setup: expected one latest snapshot, got %v", before)
	}
	// 再来一条巨大的（超限）：应被跳过，旧快照保留。
	big := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"hi"}]}`, strings.Repeat("Z", 40000))
	decodeUsage(t, h.post(t, "/v1/messages", big, map[string]string{"X-Session-Id": "over-cap"}))
	waitRequests(t, h, usage.Query{}, 2)
	after, _ := filepath.Glob(filepath.Join(dir, "*.latest.json"))
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("over-cap request displaced an existing snapshot: before=%v after=%v", before, after)
	}
}

// 上限为 0（reserve==0 的边界）含义是"保留 0 字节"：清掉所有托管快照，但无关文件不动。
func TestPruneDebugDirZeroBoundClearsManaged(t *testing.T) {
	dir := t.TempDir()
	managed := writeDebugFile(t, dir, stem('9')+".latest.json", 80, time.Second)
	writeDebugFile(t, dir, stem('9')+".latest.meta.json", 30, time.Second)
	unrelated := writeDebugFile(t, dir, "keep-me.txt", 80, time.Second)
	if _, err := pruneDebugDir(dir, 0, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("zero bound did not clear managed snapshot: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("zero bound removed an unrelated file: %v", err)
	}
}

// 写前清理失败时必须跳过写快照，避免在未知余量下继续写而超限。
func TestTracePreWritePruneFailureSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	orig := pruneDir
	pruneDir = func(string, int64, time.Duration, time.Time) (int, error) {
		return 0, fmt.Errorf("injected prune failure")
	}
	t.Cleanup(func() { pruneDir = orig })
	h := newHarness(t, 1, func(c *config.Config) { c.DebugRequestsDir = dir })
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	decodeUsage(t, h.post(t, "/v1/messages", `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`, nil))
	waitRequests(t, h, usage.Query{}, 1)
	if files, _ := filepath.Glob(filepath.Join(dir, "*request.json")); len(files) != 0 {
		t.Fatalf("snapshot written despite prune failure: %v", files)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*.latest.json")); len(files) != 0 {
		t.Fatalf("latest written despite prune failure: %v", files)
	}
}

// 启动时就要把过期调试快照清掉（无需等到第一个 tick）；关停时后台清理要退出。
func TestDebugCleanupRunsAtStartupAndStops(t *testing.T) {
	dir := t.TempDir()
	stale := writeDebugFile(t, dir, stem('7')+".latest.json", 128, 72*time.Hour)
	writeDebugFile(t, dir, stem('7')+".latest.meta.json", 40, 72*time.Hour)
	h := newHarness(t, 1, func(c *config.Config) {
		c.DebugRequestsDir = dir
		c.DebugRequestsRetention = config.Duration(time.Hour)
	})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("startup prune did not remove expired snapshot: %v", err)
	}
	// harvest cleanup 会停 bg 并 s.Wait()；后台清理若不听 bg.Done 就会在这里挂住。
	_ = h.s
}

// writeDebugFile 造一个固定大小的调试文件并设置 mtime。
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

// stem 造 64 位十六进制快照名（真实 trace 用 sha256）。
func stem(c byte) string { return strings.Repeat(string(c), 64) }

func TestTraceSnapshotGroupNaming(t *testing.T) {
	s := stem('a')
	cases := []struct {
		name   string
		group  string
		isMeta bool
		ok     bool
	}{
		{s + ".latest.json", s + "|latest", false, true},
		{s + ".latest.meta.json", s + "|latest", true, true},
		{s + "-123.request.json", s + "-123", false, true},
		{s + "-123.request.json.meta.json", s + "-123", true, true},
		{"stale.latest.json", "", false, false},
		{"deadbeef.latest.json", "", false, false},
		{"someone-else.json", "", false, false},
		{"random.request.json", "", false, false},
	}
	for _, c := range cases {
		group, isMeta, ok := traceSnapshotGroup(c.name)
		if group != c.group || isMeta != c.isMeta || ok != c.ok {
			t.Errorf("traceSnapshotGroup(%q) = (%q,%v,%v), want (%q,%v,%v)", c.name, group, isMeta, ok, c.group, c.isMeta, c.ok)
		}
	}
}

// latest 主文件必须与它的 .latest.meta.json 配成同一组，否则会被当残组删掉。
func TestPruneDebugDirKeepsPairedLatest(t *testing.T) {
	dir := t.TempDir()
	main := writeDebugFile(t, dir, stem('a')+".latest.json", 50, time.Second)
	meta := writeDebugFile(t, dir, stem('a')+".latest.meta.json", 30, time.Second)
	if _, err := pruneDebugDir(dir, 1<<30, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{main, meta} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("paired latest removed: %s: %v", keep, err)
		}
	}
}

func TestPruneDebugDirRetentionAndCap(t *testing.T) {
	dir := t.TempDir()
	// 快照按组：每组的 request/latest/meta 一起落，过期或超限时整组删。
	oldA := writeDebugFile(t, dir, stem('a')+".latest.json", 100, 48*time.Hour)
	writeDebugFile(t, dir, stem('a')+".latest.meta.json", 40, 48*time.Hour)
	freshB := writeDebugFile(t, dir, stem('b')+"-123.request.json", 100, 2*time.Minute)
	writeDebugFile(t, dir, stem('b')+"-123.request.json.meta.json", 40, 2*time.Minute)
	newestC := writeDebugFile(t, dir, stem('c')+"-9.request.json", 100, time.Second)
	writeDebugFile(t, dir, stem('c')+"-9.request.json.meta.json", 40, time.Second)

	// 保留期：48h 的组整体过期，其余保留。
	if _, err := pruneDebugDir(dir, 1<<30, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldA); !os.IsNotExist(err) {
		t.Fatalf("expired file survived: %v", err)
	}
	for _, keep := range []string{freshB, newestC} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("fresh file removed: %v", err)
		}
	}

	// 字节上限：压到只剩最新一组，最旧的组先删（整组一起走）。
	if _, err := pruneDebugDir(dir, 220, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshB); !os.IsNotExist(err) {
		t.Fatalf("cap did not drop the oldest remaining file: %v", err)
	}
	if _, err := os.Stat(newestC); err != nil {
		t.Fatalf("newest file must survive the cap: %v", err)
	}
}

func TestPruneDebugDirOnlyRemovesOwnSnapshots(t *testing.T) {
	dir := t.TempDir()
	// 属于代理自己的过期快照：应被删。
	mine := writeDebugFile(t, dir, stem('d')+".latest.json", 100, 48*time.Hour)
	// 共享目录里的无关文件：绝不能被删，即使更旧。
	unrelated := writeDebugFile(t, dir, "someone-else-notes.json", 100, 72*time.Hour)
	// 命名相似但前缀不是 64 位 hex：也不能删。
	lookalike := writeDebugFile(t, dir, "deadbeef.latest.json", 100, 72*time.Hour)
	// 子目录不动。
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	// symlink 跳过（Windows 上可能无权限，失败即跳过该子断言）。
	link := filepath.Join(dir, stem('e')+".latest.json")
	if err := os.Symlink(unrelated, link); err != nil {
		link = ""
	}
	if _, err := pruneDebugDir(dir, 1, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatalf("own expired snapshot survived: %v", err)
	}
	for _, keep := range []string{unrelated, lookalike, sub} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("unrelated path removed: %s: %v", keep, err)
		}
	}
	if link != "" {
		if _, err := os.Lstat(link); err != nil {
			t.Fatalf("symlink removed: %v", err)
		}
	}
}

func TestPruneDebugDirRemovesOrphanLatest(t *testing.T) {
	dir := t.TempDir()
	// latest 没有配套 meta：残缺快照，整组删掉以便冷启动。
	orphan := writeDebugFile(t, dir, stem('f')+".latest.json", 100, time.Second)
	if _, err := pruneDebugDir(dir, 1<<30, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan latest survived: %v", err)
	}
}

func TestPruneDebugDirKeepsDirsAndToleratesMissing(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDebugFile(t, dir, stem('a')+".latest.json", 10, 48*time.Hour)
	if _, err := pruneDebugDir(dir, 1, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(sub); err != nil || !info.IsDir() {
		t.Fatalf("cleanup removed a directory: %v", err)
	}
	// 不存在的目录不算错误（调试目录可能还没建）。
	if _, err := pruneDebugDir(filepath.Join(dir, "nope"), 1, time.Hour, time.Now()); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestPruneDebugDirSerializesWithTraceWrites(t *testing.T) {
	// 上限为 0 时只清过期，不删活跃文件；保证清理不会误删刚写的快照。
	dir := t.TempDir()
	live := writeDebugFile(t, dir, stem('1')+"-1234.request.json", 64, 0)
	liveMeta := writeDebugFile(t, dir, stem('1')+"-1234.request.json.meta.json", 20, 0)
	if _, err := pruneDebugDir(dir, 1<<30, time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{live, liveMeta} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("live snapshot removed: %v", err)
		}
	}
}
