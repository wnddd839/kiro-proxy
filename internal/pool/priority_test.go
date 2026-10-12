package pool

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/kiro"
)

func TestBreakerSuccessKeepsWindowHistory(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	if !p.Breaker().Open {
		t.Fatal("breaker not open")
	}
	p.Success("c", kiro.Usage{}, 0, false)
	if !p.Breaker().Open {
		t.Fatal("single success erased failure window")
	}
}

// 比例窗口按请求结果计数：多数成功时（即便有 2 个 distinct 号失败）不熔断。
func TestBreakerResponseRatioSuccessMajorityNotTripped(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c", "d"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	for range 4 {
		p.Success("c", kiro.Usage{}, 0, false)
		p.Success("d", kiro.Usage{}, 0, false)
	}
	// 2 次失败 / 10 次总结果 = 20% < 1/2：不熔断
	if b := p.Breaker(); b.Open {
		t.Fatalf("success majority must not trip: %+v", b)
	}
}

// 单次成功不能让 2 个 distinct 号失败刚触发的熔断解除（分母被单次成功撑大不够）。
func TestBreakerSingleSuccessDoesNotClearJustTripped(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	if !p.Breaker().Open {
		t.Fatal("want breaker open")
	}
	p.Success("c", kiro.Usage{}, 0, false)
	if !p.Breaker().Open {
		t.Fatal("single success cleared the failure window")
	}
}

// 只剩最后一个启用号时，即便窗口里还残留别的号的失败记录，也不该熔断：
// 熔断要覆盖 >=2 个启用号，最后一个号按单号逻辑走（与 last-account 保护一致）。
func TestBreakerNotOpenWithSingleEnabledAccount(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	if !p.Breaker().Open {
		t.Fatal("setup: breaker should be open with 3 enabled accounts")
	}
	// 手动停用 a、b：只剩 c 一个启用号，窗口里 a/b 的失败记录还在
	if err := p.SetDisabled("a", true, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDisabled("b", true, "manual"); err != nil {
		t.Fatal(err)
	}
	if b := p.Breaker(); b.Open {
		t.Fatalf("breaker must stay closed with a single enabled account: %+v", b)
	}
	// 再次通过失败路径查询也不应重新打开
	p.Fail("c", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	if b := p.Breaker(); b.Open {
		t.Fatalf("single enabled account must not trip breaker: %+v", b)
	}
}

// BreakerView 要能展示真实失败数/总结果数/比例，不能把总结果数误报成账号数。
func TestBreakerViewRatioFields(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Success("c", kiro.Usage{}, 0, false)
	b := p.Breaker()
	if !b.Open || len(b.Kinds) == 0 {
		t.Fatalf("breaker = %+v", b)
	}
	k := b.Kinds[0]
	if k.Accounts != 2 || k.Failures != 2 || k.Total != 3 || k.Enabled != 3 {
		t.Fatalf("breaker kind = %+v", k)
	}
	if got := k.Ratio; got < 0.66 || got > 0.67 {
		t.Fatalf("ratio = %v, want ~0.667", got)
	}
}

// 失败结果不落盘凭证；窗口历史随 sidecar 跨重启保留。
func TestBreakerWindowSurvivesRestart(t *testing.T) {
	p, path := openPool(t, nil)
	for _, id := range []string{"a", "b"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	p.Fail("a", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	p.Fail("b", kiro.Failure{Class: kiro.ClassForbidden, Status: 403})
	if !p.Breaker().Open {
		t.Fatal("want breaker open")
	}
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if !n.Breaker().Open {
		t.Fatal("failure ratio window lost across restart")
	}
}

func TestLastAccountRefreshFailureNeverDisables(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(401)
	p, _ := openPool(t, f.client())
	mustAdd(t, p, Account{ID: "only", Cred: freshCred("only")})
	for range 5 {
		_, _ = p.ForceRefresh(t.Context(), "only")
	}
	v, _ := p.Get("only")
	if v.Disabled {
		t.Fatalf("last account disabled: %+v", v)
	}
	if v.CooldownUntil.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("last account should be cooled (kept), got %+v", v)
	}
}

// 最后一个启用号被封（ClassBanned）也不停用：改长冷却并告警。
func TestLastAccountBannedNeverDisables(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "only", Cred: freshCred("only")})
	p.Fail("only", kiro.Failure{Status: 403, Message: "account suspended: TemporarilySuspended", Class: kiro.ClassBanned})
	v, _ := p.Get("only")
	if v.Disabled {
		t.Fatalf("last account disabled on ban: %+v", v)
	}
	if v.CooldownUntil.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("last account ban should long-cool, got %+v", v)
	}
	// 有两个号时可正常停用被封的
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
	p.Fail("only", kiro.Failure{Status: 403, Message: "account suspended: TemporarilySuspended", Class: kiro.ClassBanned})
	if v, _ := p.Get("only"); !v.Disabled {
		t.Fatalf("banned account not disabled when not last: %+v", v)
	}
}

// 并发 Success/Fail/Charge/Breaker 不 panic、不丢计数（-race 下验证）。
func TestStateConcurrentMutations(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	var wg sync.WaitGroup
	for i := range 60 {
		id := []string{"a", "b", "c"}[i%3]
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
	}
	wg.Wait()
	if p.Totals().Requests != 15 {
		t.Fatalf("requests = %d, want 15", p.Totals().Requests)
	}
}

// 状态 sidecar 原子落盘：并发写不会留下半个文件、重开后仍可解析。
func TestStateSidecarAtomicUnderConcurrentSaves(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() { p.saveState() })
	}
	for range 40 {
		wg.Go(func() { p.Success("a", kiro.Usage{Input: 1}, 0, false) })
	}
	wg.Wait()
	if _, err := Open(path, Options{Logger: quiet()}); err != nil {
		t.Fatalf("state sidecar unreadable after concurrent saves: %v", err)
	}
}

// 最后一个启用号刷新被封：last-account 保护的长冷却必须落盘，重启后仍在。
func TestLastAccountBannedViaRefreshCooldownPersists(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(403)
	f.refreshBody = `{"message":"Your account is TemporarilySuspended"}`
	p, path := openPool(t, f.client())
	c := freshCred("only")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "only", Cred: c})
	_, _ = p.ForceRefresh(t.Context(), "only")
	v, _ := p.Get("only")
	if v.Disabled || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("last account refresh-ban should be kept with a long cooldown: %+v", v)
	}
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ = n.Get("only")
	if v.Disabled || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("last-account ban cooldown lost across restart: %+v", v)
	}
}

// 重启后窗口内旧 strike 也必须能回退：brk.counted（每类每号已计 strike 时间）要一起持久化，
// 否则重启后累的旧 refreshFails 无法在新失败触发熔断时回退。
func TestBreakerRollbackAfterRestart(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(403)
	f.refreshBody = `{"message":"forbidden"}`
	p, path := openPool(t, f.client())
	for _, id := range []string{"a", "b", "c"} {
		c := freshCred(id)
		c.ExpiresAt = time.Now().Add(-time.Minute)
		mustAdd(t, p, Account{ID: id, Cred: c})
	}
	for range 2 { // a 触发前先失败 2 次
		_, _ = p.ForceRefresh(t.Context(), "a")
	}
	if v, _ := p.Get("a"); v.RefreshFails != 2 {
		t.Fatalf("setup a refreshFails = %d, want 2", v.RefreshFails)
	}
	n, err := Open(path, Options{Client: f.client(), Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := n.Get("a"); v.RefreshFails != 2 {
		t.Fatalf("a refreshFails after restart = %d, want 2", v.RefreshFails)
	}
	for range 2 { // b 失败直到熔断触发
		_, _ = n.ForceRefresh(t.Context(), "b")
	}
	if !n.Breaker().Open {
		t.Fatal("want breaker open")
	}
	if v, _ := n.Get("a"); v.RefreshFails != 0 {
		t.Fatalf("pre-trigger strike for a not rolled back after restart: %+v", v)
	}
}

// counted 时间过期时重启丢弃：不在本窗口内的旧 strike 记账不恢复。
func TestCountedStrikesExpireOnRestart(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	now := p.now()
	raw := `{"accounts":{"a":{"refresh_fails":2}},`
	raw += `"counted":[{"kind":"refresh","id":"a","at":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `"},`
	raw += `{"kind":"refresh","id":"a","at":"` + now.Add(-time.Second).UTC().Format(time.RFC3339Nano) + `"}]}`
	if err := os.WriteFile(path+".state.json", []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	got := n.brk.counted["refresh"]["a"]
	if len(got) != 1 {
		t.Fatalf("counted restored = %v, want only the in-window entry", got)
	}
}

// 最后一个启用号刷新报 invalid_grant（Gone）：同样要保住长冷却并落盘。
func TestLastAccountGoneViaRefreshCooldownPersists(t *testing.T) {
	f := newFakeKiro(t)
	f.refreshCode.Store(400)
	f.refreshBody = `{"error":"invalid_grant","error_description":"expired"}`
	p, path := openPool(t, f.client())
	c := freshCred("only")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "only", Cred: c})
	_, err := p.Cred(t.Context(), "only", "")
	if err == nil || !kiro.IsGone(err) {
		t.Fatalf("err = %v, want gone", err)
	}
	v, _ := p.Get("only")
	if v.Disabled || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("last account gone should be kept with a long cooldown: %+v", v)
	}
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ = n.Get("only")
	if v.Disabled || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("last-account gone cooldown lost across restart: %+v", v)
	}
}

// suspicious activity 429 是 24h 软封，不是普通 5m 限流。
func TestSuspicious429IsLongSoftBan(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	f := kiro.Classify(429, []byte(`{"message":"suspicious activity detected"}`))
	if f.Class != kiro.ClassSoftBan {
		t.Fatalf("class = %v, want ClassSoftBan", f.Class)
	}
	p.Fail("a", f)
	v, _ := p.Get("a")
	if v.CooldownUntil.Before(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("soft ban cooldown = %v, want ~24h", v.CooldownUntil)
	}
	// 普通限流仍是 5 分钟内
	p2, _ := openPool(t, nil)
	mustAdd(t, p2, Account{ID: "a", Cred: freshCred("a")})
	p2.Fail("a", kiro.Classify(429, []byte(`{"message":"Rate exceeded"}`)))
	if d := p2.slotFor(t, "a").cooldown.Sub(time.Now()); d > 6*time.Minute {
		t.Fatalf("normal throttle cooldown = %v, want <= 5m", d)
	}
}

func TestCooldownAndStatsSurviveRestart(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("secret-token-do-not-persist-in-state")})
	p.Success("a", kiro.Usage{Input: 20, Output: 4, Credits: 0.5}, 0.01, false)
	p.Fail("a", kiro.Classify(429, []byte(`{"message":"suspicious activity detected"}`)))
	n, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := n.Get("a")
	if v.Stats.Requests != 1 || v.CooldownUntil.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("state lost or soft ban treated as throttle: %+v", v)
	}
	raw, err := os.ReadFile(path + ".state.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "refresh-") {
		t.Fatalf("state contains credentials: %s", raw)
	}
}
