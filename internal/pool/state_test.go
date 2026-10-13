package pool

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/kiro"
)

// 全局熔断按窗口内请求结果的失败比例判定：多数成功不熔断；单次成功不清历史；
// 只剩一个启用号时不熔断；窗口历史跨重启保留。（403 风暴、单号 401、熔断期封号见 pool_test.go）
func TestBreakerFailureRatio(t *testing.T) {
	forbidden := kiro.Failure{Class: kiro.ClassForbidden, Status: 403}

	t.Run("success majority stays closed", func(t *testing.T) {
		p, _ := openPool(t, nil)
		for _, id := range []string{"a", "b", "c", "d"} {
			mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
		}
		p.Fail("a", forbidden)
		p.Fail("b", forbidden)
		for range 4 {
			p.Success("c", kiro.Usage{}, 0, false)
			p.Success("d", kiro.Usage{}, 0, false)
		}
		if b := p.Breaker(); b.Open { // 2 / 10
			t.Fatalf("success majority tripped: %+v", b)
		}
	})

	t.Run("trip, view, restart, last enabled", func(t *testing.T) {
		p, path := openPool(t, nil)
		for _, id := range []string{"a", "b", "c"} {
			mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
		}
		p.Fail("a", forbidden)
		p.Fail("b", forbidden)
		p.Success("c", kiro.Usage{}, 0, false)
		b := p.Breaker()
		if !b.Open || len(b.Kinds) != 1 {
			t.Fatalf("single success cleared the failure window: %+v", b)
		}
		if k := b.Kinds[0]; k.Accounts != 2 || k.Failures != 2 || k.Total != 3 || k.Enabled != 3 || k.Ratio < 0.66 || k.Ratio > 0.67 {
			t.Fatalf("breaker view = %+v", k)
		}
		if n, err := Open(path, Options{Logger: quiet()}); err != nil || !n.Breaker().Open {
			t.Fatalf("failure window lost across restart: err=%v", err)
		}
		// 停掉 a、b 后只剩 c：残留的 a/b 失败不再参与，c 自己失败也按单号逻辑走
		for _, id := range []string{"a", "b"} {
			if err := p.SetDisabled(id, true, "manual"); err != nil {
				t.Fatal(err)
			}
		}
		p.Fail("c", forbidden)
		if b := p.Breaker(); b.Open {
			t.Fatalf("single enabled account tripped the breaker: %+v", b)
		}
	})
}

// 熔断触发时回退窗口内此前计入各号的 refresh strike；这份记账跨重启保留。恰好一半不熔断。
func TestBreakerRollsBackStrikesAcrossRestart(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(403)
	f.refreshBody = `{"message":"forbidden"}`
	p, path := openPool(t, f.client())
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	for range 3 {
		p.Success("c", kiro.Usage{}, 0, false)
	}
	for range 2 {
		_, _ = p.ForceRefresh(t.Context(), "a")
	}
	p, err := Open(path, Options{Client: f.client(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Get("a"); v.RefreshFails != 2 {
		t.Fatalf("strikes lost across restart: %+v", v)
	}
	_, _ = p.ForceRefresh(t.Context(), "b")
	if p.Breaker().Open { // 3 / 6
		t.Fatal("exactly half of the results tripped")
	}
	_, _ = p.ForceRefresh(t.Context(), "b")
	if !p.Breaker().Open { // 4 / 7
		t.Fatal("majority of failed results did not trip")
	}
	for _, id := range []string{"a", "b"} {
		if v, _ := p.Get(id); v.RefreshFails != 0 || v.Disabled {
			t.Fatalf("pre-trigger strikes not rolled back: %+v", v)
		}
	}
}

// 状态 sidecar：重启恢复未过期冷却（风控软封 24h、普通限流不超过 5m）与统计，丢弃过期冷却，不存凭证。
func TestStateSidecarRestoresCooldownsAndStats(t *testing.T) {
	p, path := openPool(t, nil)
	for _, id := range []string{"soft", "throttle", "expired"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred("secret-token-" + id)})
	}
	realNow := p.now
	p.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	p.Fail("expired", kiro.Failure{Class: kiro.ClassThrottle}) // 冷却在重启前已过期
	p.now = realNow
	p.Success("soft", kiro.Usage{Input: 20, Output: 4, Credits: 0.5}, 0.01, false)
	soft := kiro.Classify(429, []byte(`{"message":"suspicious activity detected"}`))
	if soft.Class != kiro.ClassSoftBan {
		t.Fatalf("suspicious 429 class = %v", soft.Class)
	}
	p.Fail("soft", soft)
	p.Fail("throttle", kiro.Classify(429, []byte(`{"message":"Rate exceeded"}`)))

	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := n.Get("soft"); v.Stats.Requests != 1 || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("soft ban or stats lost: %+v", v)
	}
	if v, _ := n.Get("throttle"); v.CooldownUntil.IsZero() || time.Until(v.CooldownUntil) > 6*time.Minute {
		t.Fatalf("ordinary throttle cooldown = %v", time.Until(v.CooldownUntil))
	}
	if v, _ := n.Get("expired"); !v.CooldownUntil.IsZero() || v.CooldownWhy != "" || v.Stats.Errors != 1 {
		t.Fatalf("expired cooldown restored or stats dropped: %+v", v)
	}
	raw, err := os.ReadFile(path + stateSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") {
		t.Fatalf("state sidecar contains credentials: %s", raw)
	}
}

// 明确的永久失败（封号 / invalid_grant）即使是唯一的号也停用，重启后仍停用。
func TestOnlyAccountDefiniteFailureDisablesPersistently(t *testing.T) {
	for _, c := range []struct{ name, body, note string }{
		{"suspended", `{"message":"Your account is TemporarilySuspended"}`, "account suspended"},
		{"invalid_grant", `{"error":"invalid_grant","error_description":"expired"}`, "sign-in expired"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeKiro(t)
			f.refreshCode.Store(403)
			if c.name == "invalid_grant" {
				f.refreshCode.Store(400)
			}
			f.refreshBody = c.body
			p, path := openPool(t, f.client())
			cred := freshCred("only")
			cred.ExpiresAt = time.Now().Add(-time.Minute)
			mustAdd(t, p, Account{ID: "only", Cred: cred})
			_, _ = p.ForceRefresh(t.Context(), "only")
			for _, q := range []*Pool{p, nil} {
				if q == nil {
					var err error
					if q, err = Open(path, Options{Logger: quiet()}); err != nil {
						t.Fatal(err)
					}
				}
				if v, _ := q.Get("only"); !v.Disabled || !strings.Contains(v.Note, c.note) {
					t.Fatalf("definite failure not disabled persistently: %+v", v)
				}
			}
		})
	}
}

// 并发的 Success / Fail / Charge / Breaker 与落盘：不 panic（-race），sidecar 可解析，最新统计不被旧快照覆盖。
func TestStateConcurrentMutationsPersistLatest(t *testing.T) {
	p, path := openPool(t, nil)
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	var wg sync.WaitGroup
	for i := range 60 {
		id := ids[i%3]
		wg.Go(func() {
			switch i % 4 {
			case 0:
				p.Success(id, kiro.Usage{Input: 1}, 0, false)
			case 1:
				p.Fail(id, kiro.Failure{Class: kiro.ClassThrottle})
			case 2:
				p.Charge(id, kiro.Usage{Input: 1}, 0, false)
			default:
				_ = p.Breaker()
			}
		})
		wg.Go(p.saveState)
	}
	wg.Wait()
	if got := p.Totals().Requests; got != 15 {
		t.Fatalf("requests = %d, want 15", got)
	}
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatalf("state sidecar unreadable after concurrent saves: %v", err)
	}
	if got := n.Totals().Requests; got != 15 {
		t.Fatalf("restored requests = %d, want 15 (an older snapshot won)", got)
	}
}
