package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/usage"
)

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
