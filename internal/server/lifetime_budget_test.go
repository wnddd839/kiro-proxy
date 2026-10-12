package server

import (
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/usage"
)

func TestTotalBudgetStillRejectsAfterRetentionAndRestart(t *testing.T) {
	h := newHarness(t, 1, nil)
	created := time.Now().Add(-91 * 24 * time.Hour)
	st, err := keys.Open("", func() time.Time { return created })
	if err != nil {
		t.Fatal(err)
	}
	k, err := st.Create(keys.Key{Name: "lifetime", Period: keys.PeriodTotal, LimitUSD: 10})
	if err != nil {
		t.Fatal(err)
	}
	h.s.keys = st
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	j, err := usage.Open(path, usage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	j.Record(usage.Entry{Key: k.ID, Time: created, Status: 200, CostUSD: 10})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		j, err = usage.Open(path, usage.Options{})
		if err != nil {
			t.Fatal(err)
		}
		h.s.usage = j
		res := h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": k.Secret})
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("restart %d: exhausted budget status=%d", i, res.StatusCode)
		}
		view := h.s.keyView(k, time.Now())
		if view.Spent.CostUSD != 10 || view.Remaining == nil || *view.Remaining != 0 {
			t.Fatalf("budget display lost history: %+v", view)
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
