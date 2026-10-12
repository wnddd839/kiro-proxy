package meter

import (
	"testing"
	"time"

	"kiro-proxy/internal/turn"
)

func TestCalibrationDoesNotInventInputOrScaleOutput(t *testing.T) {
	u := turn.Usage{CacheRead: 1001, CacheWrite: 999, Output: 123, Reasoning: 31}
	got := Calibrate(u, 4052+1801+123, 4052)
	if got.Input != 0 || got.Output != u.Output || got.Reasoning != u.Reasoning || got.CacheRead >= u.CacheRead {
		t.Fatalf("calibration changed output or invented uncached input: %+v", got)
	}
}

func TestMissingOnlineCachePricesUseBuiltin(t *testing.T) {
	m, err := ParseOpenRouter([]byte(`{"data":[{"id":"anthropic/claude-sonnet-4.5","pricing":{"prompt":"0.000003","completion":"0.000015"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	p := &Pricer{}
	p.SetOnline(m, time.Now())
	pr, _ := p.PriceOf("claude-sonnet-4.5")
	if pr.CacheRead != 0.3 || pr.CacheWrite != 3.75 || pr.CacheWrite1h != 6 {
		t.Fatalf("missing cache price overcharges: %+v", pr)
	}
	unknown, ok := p.PriceOf("claude-opus-99")
	if ok || unknown.Input < 15 || unknown.Output < 75 {
		t.Fatalf("unknown model is underpriced: %+v matched=%v", unknown, ok)
	}
}

func TestMissingOnlineLongCachePricesUseBuiltinTier(t *testing.T) {
	p := &Pricer{}
	p.SetOnline(map[string]Price{"gpt-5.6-sol": {Input: 8, Output: 40, CacheRead: 0.8, CacheWrite: 8, LongAbove: 272000, Long: &Price{Input: 20, Output: 100}}}, time.Now())
	pr, _ := p.PriceOf("gpt-5.6-sol")
	if pr.Long.CacheRead != 0.4 || pr.Long.CacheWrite != 5 || pr.Warning == "" {
		t.Fatalf("long missing caches inferred instead of builtin fallback: %+v long=%+v", pr, pr.Long)
	}
}
