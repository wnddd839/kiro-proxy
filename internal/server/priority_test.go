package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

func TestHiddenTokensAreSeparateOnWireAndInJournal(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	res := h.post(t, "/v1/messages", convo("hi"), nil)
	defer res.Body.Close()
	var msg struct{ Usage map[string]any }
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Usage["input_tokens"] != float64(0) || msg.Usage["hidden_tokens"] != float64(4052) {
		t.Fatalf("hidden input leaked into client input: %+v", msg.Usage)
	}
	v := h.s.usage.Query(usage.Query{})
	b, _ := json.Marshal(v.Entries[0])
	var e map[string]any
	_ = json.Unmarshal(b, &e)
	if e["hidden_tokens"] != float64(4052) || e["hidden_tokens_charged_usd"] != 4052*3.0/1e6 {
		t.Fatalf("missing independent hidden accounting: %s", b)
	}
}

func TestEightRoundUsageScenarios(t *testing.T) {
	for _, scenario := range []string{"hit", "cold", "miss"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, 1, func(c *config.Config) {
				if scenario == "miss" {
					c.CacheMode = meter.ModeOff
				}
			})
			var messages []anthropic.Message
			previous := 0
			for round := 1; round <= 8; round++ {
				target := 1700 + (round-1)*10800/7
				// Binary-search real local tokens; the simulator still sees real growing message history.
				lo, hi := 0, target*3
				req := anthropic.Request{Model: "claude-sonnet-4.5", MaxTokens: 1000}
				for lo < hi {
					n := (lo + hi) / 2
					req.Messages = append(append([]anthropic.Message{}, messages...), anthropic.Message{Role: "user", Content: anthropic.Content{{Type: "text", Text: strings.Repeat(" a", n)}}})
					p := meter.Analyze(&req, "", 0, meter.ModeOff)
					if p.Tokens < target {
						lo = n + 1
					} else {
						hi = n
					}
				}
				req.Messages = append(append([]anthropic.Message{}, messages...), anthropic.Message{Role: "user", Content: anthropic.Content{{Type: "text", Text: strings.Repeat(" a", lo)}}})
				prompt := meter.Analyze(&req, "", 0, meter.ModeOff).Tokens
				answer := strings.Repeat("answer ", round*10)
				output := meter.Count(answer)
				h.up.stream = func(string) []byte {
					b := frame("assistantResponseEvent", fmt.Sprintf(`{"content":%q}`, answer))
					return append(b, frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(prompt+4052+output)/2000))...)
				}
				if scenario == "cold" {
					h.s.cache.Forget("a0")
				}
				body, _ := json.Marshal(req)
				u := decodeUsage(t, h.post(t, "/v1/messages", string(body), map[string]string{"X-Session-Id": "eight-rounds"}))
				e := waitRequests(t, h, usage.Query{}, int64(round)).Entries[0]
				if u.Input+u.CacheRead+u.CacheWrite != prompt || u.Output != output || e.HiddenTokens != 4052 || u.Input == 4052 || e.HiddenChargedUSD != 0.012156 {
					t.Fatalf("round %d: usage=%+v journal=%+v prompt=%d", round, u, e, prompt)
				}
				if scenario == "hit" && round > 1 && (u.CacheRead != previous || u.CacheWrite <= 0) {
					t.Fatalf("prefix not reused: %+v previous=%d", u, previous)
				}
				if scenario == "cold" && (u.CacheRead != 0 || u.CacheWrite != prompt) {
					t.Fatalf("cold split: %+v", u)
				}
				if scenario == "miss" && (u.Input != prompt || u.CacheRead != 0 || u.CacheWrite != 0) {
					t.Fatalf("miss split: %+v", u)
				}
				oldInput := u.Input + 4052
				t.Logf("round=%d prompt=%d before_input=%d after_input=%d read=%d write=%d output=%d hidden=%d hidden_usd=%.6f", round, prompt, oldInput, u.Input, u.CacheRead, u.CacheWrite, u.Output, e.HiddenTokens, e.HiddenChargedUSD)
				previous = prompt
				messages = append(req.Messages, anthropic.Message{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: answer}}})
			}
		})
	}
}

func TestHiddenBillingOffAndOverride(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) {
		c.HiddenTokensBilling = "off"
		c.HiddenTokens = map[string]int{"claude-sonnet-4.5": 4321}
	})
	h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
	decodeUsage(t, h.post(t, "/v1/messages", convo("hi"), nil))
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.HiddenTokens != 4321 || e.HiddenChargedUSD != 0 || e.Input != 0 {
		t.Fatalf("hidden billing off: %+v", e)
	}
	if e.CostUSD != h.s.pricer.APICost(e.Model, turn.Usage{Input: e.Input, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Output: e.Output, KiroInput: e.HiddenTokens}) {
		t.Fatalf("off still billed: %+v", e)
	}
	h.s.models.merge([]kiro.Model{{ID: "claude-opus-5.5"}})
	for _, r := range h.s.modelRows() {
		if r.ID == "claude-opus-5.5" && (r.HiddenCalibration != "未校准" || r.HiddenTokens != 4052) {
			t.Fatalf("invented calibration: %+v", r)
		}
	}
}

func TestUnknownModelPricingPolicyHTTP(t *testing.T) {
	for _, policy := range []string{"highest", "reject"} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t, 1, func(c *config.Config) { c.UnknownModelPricing = policy })
			h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
			body := strings.Replace(convo("hi"), "claude-sonnet-4.5", "claude-opus-99", 1)
			res := h.post(t, "/v1/messages", body, nil)
			defer res.Body.Close()
			if policy == "reject" {
				if res.StatusCode != http.StatusBadRequest || len(h.up.tokens()) != 0 {
					t.Fatalf("unknown model entered upstream: status=%d calls=%v", res.StatusCode, h.up.tokens())
				}
				return
			}
			if res.StatusCode != http.StatusOK {
				t.Fatalf("highest policy rejected: %d", res.StatusCode)
			}
			io.Copy(io.Discard, res.Body)
			e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
			pr, known := h.s.pricer.PriceOf(e.Model)
			if known || pr.Warning == "" || pr.Input < 15 || pr.Output < 75 || e.HiddenChargedUSD != float64(4052)*pr.Input/1e6 {
				t.Fatalf("unknown model underpriced or unwarned: price=%+v entry=%+v", pr, e)
			}
		})
	}
}

func TestPreContentReportedFailureSeparatesHidden(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, mode := range []string{kiro.ReportedConservative, kiro.ReportedRaw} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, mode), func(t *testing.T) {
				h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = mode })
				h.up.stream = func(string) []byte {
					b := frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":5286,"outputTokens":0}}`)
					return append(b, frame("validationError", `{"message":"Input is too long"}`)...)
				}
				body := convo("hi")
				if streaming {
					body = strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1)
				}
				res := h.post(t, "/v1/messages", body, nil)
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
				if res.StatusCode != http.StatusBadRequest || e.Input != 1234 || e.HiddenTokens != 4052 || e.HiddenChargedUSD != 0.012156 {
					t.Fatalf("pre-content report leaked hidden input: status=%d entry=%+v", res.StatusCode, e)
				}
			})
		}
	}
}

func TestConcurrentUSDReservationsSettleAndRelease(t *testing.T) {
	h := newHarness(t, 6, nil)
	c := &call{model: "claude-sonnet-4.5", prompt: meter.Analyze(&anthropic.Request{System: anthropic.Content{{Type: "text", Text: bigSystem}}, Messages: []anthropic.Message{{Role: "user", Content: anthropic.Content{{Type: "text", Text: "hi"}}}}}, "", 0, meter.ModeAuto)}
	c.req.MaxTokens = 100
	reserve := h.s.estimate(c, usage.Totals{}).CostUSD
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

func TestStreamStartEstimateAndActualSettlement(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":0,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":1}}`)...)
	}
	res := h.post(t, "/v1/messages", strings.Replace(convo("hi"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1), nil)
	defer res.Body.Close()
	var start, end, actual map[string]any
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string
			Message struct{ Usage map[string]any }
			Usage   map[string]any
			Actual  map[string]any `json:"settled_usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "message_start" {
			start = ev.Message.Usage
		}
		if ev.Type == "message_delta" {
			end, actual = ev.Usage, ev.Actual
		}
	}
	if start == nil || end == nil || actual == nil || start["cache_creation_input_tokens"].(float64) <= 0 {
		t.Fatalf("no start estimate: %v %v %v", start, end, actual)
	}
	for _, field := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "hidden_tokens", "output_tokens"} {
		if end[field].(float64) < start[field].(float64) {
			t.Fatalf("usage regressed: %s", field)
		}
	}
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.Input+e.CacheRead+e.CacheWrite != 0 || e.HiddenTokens != 0 || e.CostUSD != 15.0/1e6 || actual["cache_creation_input_tokens"] != float64(0) {
		t.Fatalf("display estimate overbilled: %+v actual=%v", e, actual)
	}
}

func TestAdmissionIncludesCurrentReservation(t *testing.T) {
	h := newHarness(t, 1, nil)
	k := &keys.Key{ID: "budget", Name: "budget", LimitUSD: 0.001, CreatedAt: time.Now()}
	c := &call{key: k, model: "claude-sonnet-4.5", prompt: &meter.Prompt{Tokens: 2000}}
	c.req.Model, c.req.MaxTokens = c.model, 10000
	status, err := h.s.admit(httptest.NewRecorder(), c)
	if err == nil || status != http.StatusPaymentRequired {
		t.Fatalf("current request bypasses budget: status=%d error=%v hold=%+v", status, err, c.hold)
	}
	if got := h.s.holds.reserved(k.ID); got.Requests != 0 {
		t.Fatalf("rejection leaked reservation: %+v", got)
	}
}

func TestReservationCoversExpensiveCacheAndLongTier(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.s.pricer.Overrides = map[string]meter.Price{"claude-sonnet-4.5": {Input: 3, Output: 15, CacheWrite: 5, CacheWrite1h: 20, LongAbove: 4000, Long: &meter.Price{Input: 8, Output: 25, CacheWrite1h: 50}}}
	c := &call{model: "claude-sonnet-4.5", prompt: &meter.Prompt{Tokens: 2000}}
	c.req.MaxTokens = 100
	actual := turn.Usage{CacheWrite: 4000, CacheWrite1h: 4000, KiroInput: 4052, Output: 100}
	reserve := h.s.estimate(c, usage.Totals{}).CostUSD
	if want := h.s.pricer.APICost(c.model, actual); reserve < want {
		t.Fatalf("reservation %.8f < worst cache/long settlement %.8f", reserve, want)
	}
}
