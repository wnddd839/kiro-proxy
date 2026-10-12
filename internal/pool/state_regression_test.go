package pool

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/kiro"
)

func TestExpiredCooldownDiscardedButStatisticsKept(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	p.Success("a", kiro.Usage{Input: 4}, .01, false)
	p.Fail("a", kiro.Failure{Class: kiro.ClassThrottle})
	raw, err := os.ReadFile(path + stateSuffix)
	if err != nil {
		t.Fatal(err)
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	a := st.Accounts["a"]
	a.CooldownUntil = time.Now().Add(-time.Hour)
	st.Accounts["a"] = a
	raw, _ = json.Marshal(st)
	if err := os.WriteFile(path+stateSuffix, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p2, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p2.Get("a")
	if !v.CooldownUntil.IsZero() || v.CooldownWhy != "" || v.Stats.Requests != 1 || v.Stats.Errors != 1 {
		t.Fatalf("expired cooldown/statistics restore: %+v", v)
	}
}

func TestConcurrentStatePersistenceKeepsLatestStats(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() { p.Success("a", kiro.Usage{Input: 1}, 0, false) })
	}
	wg.Wait()
	p2, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := p2.Get("a")
	if v.Stats.Requests != 40 || v.Stats.InputTokens != 40 {
		t.Fatalf("old snapshot overwrote current statistics: %+v", v)
	}
}

func TestBreakerRollsBackActualPreTriggerRefreshFailures(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(403)
	f.refreshBody = `{"message":"forbidden"}`
	p, _ := openPool(t, f.client())
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	for range 3 {
		p.Success("c", kiro.Usage{}, 0, false)
	}
	for range 2 {
		_, _ = p.ForceRefresh(t.Context(), "a")
	}
	if v, _ := p.Get("a"); v.RefreshFails != 2 {
		t.Fatalf("setup did not create pre-trigger strikes: %+v", v)
	}
	_, _ = p.ForceRefresh(t.Context(), "b")
	if p.Breaker().Open {
		t.Fatal("exactly half of results should not trip")
	}
	_, _ = p.ForceRefresh(t.Context(), "b")
	if !p.Breaker().Open {
		t.Fatal("majority of failed results must trip")
	}
	for _, id := range []string{"a", "b"} {
		if v, _ := p.Get(id); v.RefreshFails != 0 || v.Disabled {
			t.Fatalf("pre-trigger strike not rolled back: %+v", v)
		}
	}
}
