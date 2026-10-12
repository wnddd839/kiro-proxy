package server

import (
	"io"
	"net/http"
	"testing"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/usage"
)

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
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.CostUSD != 0.002 {
		t.Fatalf("credits settlement=%g", e.CostUSD)
	}
}

func TestShortRequestDoesNotReserveUnrelatedTiersOrHistory(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) {
		c.CacheMode = meter.ModeOff
		c.Prices = map[string]meter.Price{"claude-sonnet-4.5": {Input: 3, Output: 15, CacheWrite1h: 500, LongAbove: 100000, Long: &meter.Price{Input: 100, Output: 100}}}
	})
	k, err := h.s.keys.Create(keys.Key{Name: "short", LimitUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	h.s.usage.Record(usage.Entry{Key: k.ID, Status: 200, CostUSD: 0.99})
	body := `{"model":"claude-sonnet-4.5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	res := h.post(t, "/v1/messages", body, map[string]string{"x-api-key": k.Secret})
	io.Copy(io.Discard, res.Body)
	// Disable hidden input charges to isolate prompt/output reservation below the remaining budget.
	if res.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("hidden baseline should exceed remaining budget: %d", res.StatusCode)
	}
	h.s.pricer.HiddenBilling = "off"
	res = h.post(t, "/v1/messages", body, map[string]string{"x-api-key": k.Secret})
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("short request used expensive history/context/TTL: %d", res.StatusCode)
	}
}
