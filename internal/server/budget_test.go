package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// 并发准入互相看得见预留：额度只够两条时，6 条并发里恰好放行两条，结算后释放预留且不超限。
func TestConcurrentUSDReservationsSettleAndRelease(t *testing.T) {
	h := newHarness(t, 6, nil)
	c := &call{model: "claude-sonnet-4.5", prompt: meter.Analyze(&anthropic.Request{System: anthropic.Content{{Type: "text", Text: bigSystem}}, Messages: []anthropic.Message{{Role: "user", Content: anthropic.Content{{Type: "text", Text: "hi"}}}}}, "", 0, meter.ModeAuto)}
	c.req.MaxTokens = 100
	reserve := h.s.estimate(c).CostUSD
	k, err := h.s.keys.Create(keys.Key{Name: "concurrent", LimitUSD: reserve*2 + 1e-9})
	if err != nil {
		t.Fatal(err)
	}
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
			w.Write(frame("assistantResponseEvent", `{"content":"ok"}`))
		case <-r.Context().Done():
		}
	}
	codes := make(chan int, 6)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.srv.URL+"/v1/messages", strings.NewReader(convo("hi")))
			req.Header.Set("x-api-key", k.Secret)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			codes <- res.StatusCode
		})
	}
	for range 4 {
		select {
		case status := <-codes:
			if status != 402 {
				t.Fatalf("unreserved request entered: %d", status)
			}
		case <-time.After(5 * time.Second):
			release()
			t.Fatal("concurrent requests bypassed reservation")
		}
	}
	release()
	wg.Wait()
	for range 2 {
		if status := <-codes; status != 200 {
			t.Fatalf("reserved request failed: %d", status)
		}
	}
	v := waitRequests(t, h, usage.Query{}, 2)
	if v.Totals.CostUSD > k.LimitUSD || h.s.holds.reserved(k.ID).Requests != 0 {
		t.Fatalf("settlement leaked or exceeded: %+v", v.Totals)
	}
}

// 准入把本请求自己的预留也算进去，被拒时不留预留；短请求只按本请求适用的档位预留，
// 不叠加用不到的长上下文 / 1h 价或历史平均。
func TestAdmissionReservesThisRequestOnly(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) {
		c.CacheMode = meter.ModeOff
		c.Prices = map[string]meter.Price{"claude-sonnet-4.5": {Input: 3, Output: 15, CacheWrite1h: 500, LongAbove: 100000, Long: &meter.Price{Input: 100, Output: 100}}}
	})
	tight, err := h.s.keys.Create(keys.Key{Name: "tight", LimitUSD: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	res := h.post(t, "/v1/messages", `{"model":"claude-sonnet-4.5","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"x-api-key": tight.Secret})
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusPaymentRequired || h.s.holds.reserved(tight.ID).Requests != 0 || len(h.up.tokens()) != 0 {
		t.Fatalf("this request's reservation not checked or leaked: status=%d hold=%+v", res.StatusCode, h.s.holds.reserved(tight.ID))
	}

	short, err := h.s.keys.Create(keys.Key{Name: "short", LimitUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.s.usage.Record(usage.Entry{Key: short.ID, Status: 200, CostUSD: 0.99})
	body := `{"model":"claude-sonnet-4.5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	res = h.post(t, "/v1/messages", body, map[string]string{"x-api-key": short.Secret})
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("hidden baseline should exceed the remaining budget: %d", res.StatusCode)
	}
	h.s.pricer.HiddenBilling = "off" // 只剩 prompt + 10 output，远低于余额
	res = h.post(t, "/v1/messages", body, map[string]string{"x-api-key": short.Secret})
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("short request reserved unrelated tiers or history: %d", res.StatusCode)
	}
}

// 预留覆盖请求声明的 1h 缓存写入与长上下文档位（结算不会高于预留）。
func TestReservationCoversRequestedCacheTTLAndContextTier(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.s.pricer.Overrides = map[string]meter.Price{"claude-sonnet-4.5": {Input: 3, Output: 15, CacheWrite: 5, CacheWrite1h: 20, LongAbove: 4000, Long: &meter.Price{Input: 8, Output: 25, CacheWrite1h: 50}}}
	prompt := meter.Analyze(&anthropic.Request{System: anthropic.Content{{Type: "text", Text: strings.Repeat(" a", 2000), CacheControl: &anthropic.CacheControl{Type: "ephemeral", TTL: "1h"}}}}, "", 0, meter.ModeExplicit)
	c := &call{model: "claude-sonnet-4.5", prompt: prompt}
	c.req.MaxTokens = 100
	actual := turn.Usage{CacheWrite: prompt.Tokens, CacheWrite1h: prompt.Tokens, Output: 100}.WithHidden(4052)
	if reserve, want := h.s.estimate(c).CostUSD, h.s.pricer.APICost(c.model, actual); reserve < want {
		t.Fatalf("reservation %.8f < requested cache/long settlement %.8f", reserve, want)
	}
}

// credits 口径：准入与结算都按 credits × credit_usd，不被无关的 API 标价拒绝。
func TestCreditsBudgetAdmissionUsesCreditsPrices(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.CostBasis = "credits" })
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("meteringEvent", `{"usage":0.1,"unit":"credit"}`)...)
	}
	k, err := h.s.keys.Create(keys.Key{Name: "credits", LimitUSD: 0.005})
	if err != nil {
		t.Fatal(err)
	}
	res := h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": k.Secret})
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("request rejected by unrelated API prices: %d", res.StatusCode)
	}
	if e := waitRequests(t, h, usage.Query{}, 1).Entries[0]; e.CostUSD != 0.002 {
		t.Fatalf("credits settlement=%g", e.CostUSD)
	}
}

// 上游没报 credits：按 token 估算，响应、账本与号上统计用同一个结算值。
func TestMissingCreditsSettleConsistently(t *testing.T) {
	for _, basis := range []string{"api", "credits"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", basis, streaming), func(t *testing.T) {
				h := newHarness(t, 1, func(c *config.Config) { c.CostBasis = basis })
				res := h.post(t, "/v1/messages", strings.Replace(convo("hi"), `"max_tokens":100`, fmt.Sprintf(`"max_tokens":100,"stream":%v`, streaming), 1), nil)
				var got struct {
					Credits float64 `json:"credits"`
				}
				if streaming {
					sc := bufio.NewScanner(res.Body)
					for sc.Scan() {
						data, ok := strings.CutPrefix(sc.Text(), "data: ")
						if !ok {
							continue
						}
						var ev struct {
							Type  string
							Usage json.RawMessage
						}
						if err := json.Unmarshal([]byte(data), &ev); err != nil {
							t.Fatal(err)
						}
						if ev.Type == "message_delta" {
							if err := json.Unmarshal(ev.Usage, &got); err != nil {
								t.Fatal(err)
							}
						}
					}
					if err := sc.Err(); err != nil {
						t.Fatal(err)
					}
				} else {
					var msg struct{ Usage json.RawMessage }
					if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(msg.Usage, &got); err != nil {
						t.Fatal(err)
					}
				}
				e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
				stats := h.pool.Totals()
				deadline := time.Now().Add(time.Second)
				for stats.Requests == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
					stats = h.pool.Totals()
				}
				if got.Credits <= 0 || got.Credits != e.Credits || !e.CreditsEstimated {
					t.Fatalf("wire=%+v journal=%+v", got, e)
				}
				if math.Abs(stats.Credits-e.Credits) > 1e-12 || math.Abs(stats.CostUSD-e.CostUSD) > 1e-12 {
					t.Fatalf("pool=%+v journal=%+v", stats, e)
				}
				if basis == "credits" && math.Abs(e.CostUSD-e.Credits*h.s.cfg.CreditUSD) > 1e-12 {
					t.Fatalf("wrong credits cost: %+v", e)
				}
			})
		}
	}
}

// total 周期预算不随明细保留期清零：超过保留期且多次重启后仍拒绝；换号重试不计入；
// 检查点之后的新支出照常累加，重启不重复计入检查点。
func TestTotalBudgetSurvivesRetentionAndRestart(t *testing.T) {
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
	open := func() *usage.Journal {
		t.Helper()
		j, err := usage.Open(path, usage.Options{})
		if err != nil {
			t.Fatal(err)
		}
		h.s.usage = j
		return j
	}
	j := open()
	j.Record(usage.Entry{Key: k.ID, Time: created, Status: 200, CostUSD: 10, Credits: 2})
	j.Record(usage.Entry{Key: k.ID, Time: created, Status: 502, Retried: true, Credits: 3})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	for restart := range 2 {
		j = open()
		res := h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": k.Secret})
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("restart %d: exhausted budget status=%d", restart, res.StatusCode)
		}
		view := h.s.keyView(k, time.Now())
		if view.Spent.Requests != 1 || view.Spent.CostUSD != 10 || view.Spent.Credits != 2 || view.Remaining == nil || *view.Remaining != 0 {
			t.Fatalf("restart %d: budget lost history or counted the retry: %+v", restart, view.Spent)
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
	}
	j = open()
	j.Record(usage.Entry{Key: k.ID, Time: time.Now(), Status: 200, CostUSD: 1, Credits: 0.5})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = open()
	defer j.Close()
	if got := j.LifetimeSpend(k.ID); got.Requests != 2 || got.CostUSD != 11 || got.Credits != 2.5 {
		t.Fatalf("checkpoint double counted or new spend lost: %+v", got)
	}
}

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

	res := h.post(t, "/v1/messages", convo("hi"), nil)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("anonymous request refused during disk failure: %d", res.StatusCode)
	}

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
