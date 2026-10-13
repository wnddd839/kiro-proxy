package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/usage"
)

// 账本写盘失败：匿名（本机自用）照常服务、记录留在内存；带预算的 key 暂停接单；/health 报 503；恢复后不丢不重。
func TestJournalWriteFailurePausesBudgetedKeysAndRecovers(t *testing.T) {
	h := newHarness(t, 1, nil)
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	journal, err := usage.Open(path, usage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	h.s.usage = journal
	// A directory at the file path produces a real write failure on both supported OSes.
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	journal.Record(usage.Entry{ID: "before-failure", Time: time.Now(), Status: 200, CostUSD: 1})
	if err := journal.Flush(); err == nil {
		t.Fatal("disk fault did not occur")
	}

	// 匿名：没有预算要守，账本故障不拦生成
	res := h.post(t, "/v1/messages", convo("hi"), nil)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("anonymous request refused during disk failure: %d", res.StatusCode)
	}

	// 带预算的 key：看不到最新支出就不能准入
	k, err := h.s.keys.Create(keys.Key{Name: "team", LimitUSD: 10})
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"x-api-key": k.Secret}
	res = h.post(t, "/v1/messages", convo("hi"), auth)
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "usage journal unavailable") || len(h.up.tokens()) != 1 {
		t.Fatalf("budgeted request accepted during disk failure: %d %s", res.StatusCode, raw)
	}
	health, err := http.Get(h.srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health hid disk failure: %d", health.StatusCode)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := journal.Flush(); err != nil {
		t.Fatal(err)
	}
	res = h.post(t, "/v1/messages", convo("hi"), auth)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("did not resume: %d", res.StatusCode)
	}
	waitRequests(t, h, usage.Query{}, 3)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := usage.Open(path, usage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	// before-failure + 故障期间的匿名请求 + 恢复后的 key 请求；被拒的那次不入账
	if v := restored.Query(usage.Query{}); v.Totals.Requests != 3 || v.TotalEntries != 3 {
		t.Fatalf("accepted records lost or duplicated: %+v", v)
	}
}
