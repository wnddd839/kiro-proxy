package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLifetimeSpendSurvivesRetentionAndRestarts(t *testing.T) {
	c := newClock(at(12, 0))
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	opts := Options{Now: c.Now, Location: utc8, Retention: 24 * time.Hour}
	j, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	e := entry(c.Now(), "member", "model", 200, 10, 1, 10)
	e.Credits = 2
	j.Record(e)
	e.Retried = true
	e.CostUSD = 0
	e.Credits = 3
	j.Record(e)
	c.Add(48 * time.Hour)
	j.prune()
	for i := 0; i < 3; i++ {
		total := j.LifetimeSpend("member")
		if total.Requests != 1 || total.CostUSD != 10 || total.BillableCredits() != 2 {
			t.Fatalf("restart %d: %+v", i, total)
		}
		if j.Query(Query{}).Totals.Requests != 0 {
			t.Fatal("expired details retained")
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		j, err = Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer j.Close()
	e = entry(c.Now(), "member", "model", 200, 10, 1, 1)
	e.Credits = 0.5
	j.Record(e)
	if got := j.LifetimeSpend("member"); got.Requests != 2 || got.CostUSD != 11 || got.Credits != 2.5 {
		t.Fatalf("new spending not added: %+v", got)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.LifetimeSpend("member"); got.Requests != 2 || got.CostUSD != 11 || got.Credits != 2.5 {
		t.Fatalf("checkpoint double counted: %+v", got)
	}
}
