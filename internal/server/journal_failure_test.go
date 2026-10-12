package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/usage"
)

func TestJournalWriteFailurePausesRequestsAndRecovers(t *testing.T) {
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
	res := h.post(t, "/v1/messages", convo("hi"), nil)
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "usage journal unavailable") || len(h.up.tokens()) != 0 {
		t.Fatalf("request accepted during disk failure: %d %s", res.StatusCode, raw)
	}
	health, err := http.Get(h.srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != 503 {
		t.Fatalf("health hid disk failure: %d", health.StatusCode)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := journal.Flush(); err != nil {
		t.Fatal(err)
	}
	res = h.post(t, "/v1/messages", convo("hi"), nil)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("did not resume: %d", res.StatusCode)
	}
	waitRequests(t, h, usage.Query{}, 2)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := usage.Open(path, usage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	v := restored.Query(usage.Query{})
	if v.Totals.Requests != 2 || v.TotalEntries != 2 {
		t.Fatalf("accepted records lost or duplicated: %+v", v)
	}
}
