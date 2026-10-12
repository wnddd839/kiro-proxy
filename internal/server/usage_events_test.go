package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// wireUsage 是响应 usage 的线上形状，含独立的隐藏 token 账。
type wireUsage struct {
	usageBody
	HiddenTokens int `json:"hidden_tokens"`
}

func decodeWire(t *testing.T, res *http.Response) wireUsage {
	t.Helper()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	var msg struct{ Usage wireUsage }
	if err := json.NewDecoder(res.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	return msg.Usage
}

func TestFirstReportedStreamUsageDoesNotOverbill(t *testing.T) {
	hidden := kiro.HiddenTokens("claude-sonnet-4.5")
	for _, mode := range []string{kiro.ReportedConservative, kiro.ReportedRaw, kiro.ReportedSum} {
		for _, input := range []int{0, 10} {
			t.Run(fmt.Sprintf("%s/input=%d", mode, input), func(t *testing.T) {
				h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = mode })
				h.up.stream = func(string) []byte {
					b := frame("assistantResponseEvent", `{"content":"ok"}`)
					// 上游报的输入是整段上下文：客户端 input 之外还含 Kiro 隐藏基线。
					return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"cacheReadInputTokens":6000,"outputTokens":3}}`, input+hidden))...)
				}
				body := strings.Replace(convo("hi"), `"messages"`, `"stream":true,"messages"`, 1)
				res := h.post(t, "/v1/messages", body, nil)
				var start, end wireUsage
				sawStart, sawEnd := false, false
				sc := bufio.NewScanner(res.Body)
				for sc.Scan() {
					data, ok := strings.CutPrefix(sc.Text(), "data: ")
					if !ok {
						continue
					}
					var ev struct {
						Type    string
						Message struct{ Usage wireUsage }
						Usage   wireUsage
					}
					if err := json.Unmarshal([]byte(data), &ev); err != nil {
						t.Fatal(err)
					}
					switch ev.Type {
					case "message_start":
						start, sawStart = ev.Message.Usage, true
					case "message_delta":
						end, sawEnd = ev.Usage, true
					}
				}
				if err := sc.Err(); err != nil || !sawStart || !sawEnd {
					t.Fatalf("stream usage missing: %v", err)
				}
				// sub2api only overwrites message_delta input when it is positive.
				mergedInput := start.Input
				if end.Input > 0 {
					mergedInput = end.Input
				}
				if start.Input > end.Input || mergedInput != input || end.HiddenTokens != hidden {
					t.Fatalf("stream overbilled: start=%+v end=%+v merged=%d want=%d hidden_want=%d", start, end, mergedInput, input, hidden)
				}
			})
		}
	}
}

func TestReportedOutputSurvivesContextCalibration(t *testing.T) {
	h := newHarness(t, 1, nil)
	body := convo("hi")
	res := h.post(t, "/v1/messages/count_tokens", body, nil)
	var count struct {
		Input int `json:"input_tokens"`
	}
	if err := json.NewDecoder(res.Body).Decode(&count); err != nil {
		t.Fatal(err)
	}
	hidden := kiro.HiddenTokens("claude-sonnet-4.5")
	upstream := hidden + count.Input*4/5 + 11
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"hello world"}`)
		b = append(b, frame("metadataEvent", `{"tokenUsage":{"outputTokens":11}}`)...)
		return append(b, frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(upstream)/2000))...)
	}
	u := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
	if u.Output != 11 || u.Input+u.CacheRead+u.CacheWrite+u.Output < upstream-hidden-2 || u.Input+u.CacheRead+u.CacheWrite+u.Output > upstream-hidden {
		t.Fatalf("reported output rescaled: usage=%+v upstream=%d", u, upstream)
	}
}

func TestIgnoredReportsStillCalibrateContext(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = kiro.ReportedIgnore })
	body := convo("hi")
	res := h.post(t, "/v1/messages/count_tokens", body, nil)
	var count struct {
		Input int `json:"input_tokens"`
	}
	if err := json.NewDecoder(res.Body).Decode(&count); err != nil {
		t.Fatal(err)
	}
	hidden := kiro.HiddenTokens("claude-sonnet-4.5")
	upstream := hidden + (count.Input+meter.Count("hello world"))*4/5
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"hello world"}`)
		b = append(b, frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":999999,"outputTokens":9999}}`)...)
		return append(b, frame("contextUsageEvent", fmt.Sprintf(`{"contextUsagePercentage":%v}`, float64(upstream)/2000))...)
	}
	u := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
	if u.Input != 0 || u.Output == 9999 || u.Input+u.CacheRead+u.CacheWrite+u.Output < upstream-hidden-2 || u.Input+u.CacheRead+u.CacheWrite+u.Output > upstream-hidden {
		t.Fatalf("ignore failed to calibrate local usage: usage=%+v upstream=%d", u, upstream)
	}
}

// 上报的输入是整段上下文：客户端 input 应为 total−hidden，隐藏账独立记在 hidden_tokens / ledger。
func TestReportedInputCarvesHiddenSeparately(t *testing.T) {
	hidden := kiro.HiddenTokens("claude-sonnet-4.5")
	for _, mode := range []string{kiro.ReportedConservative, kiro.ReportedRaw} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = mode })
			const clientInput = 1234
			h.up.stream = func(string) []byte {
				b := frame("assistantResponseEvent", `{"content":"ok"}`)
				return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"cacheReadInputTokens":700,"cacheWriteInputTokens":300,"outputTokens":5}}`, clientInput+hidden))...)
			}
			u := decodeWire(t, h.post(t, "/v1/messages", convo("hi"), nil))
			if u.Input != clientInput || u.CacheRead != 700 || u.CacheWrite != 300 || u.HiddenTokens != hidden {
				t.Fatalf("carve wrong: %+v want input=%d read=700 write=300 hidden=%d", u, clientInput, hidden)
			}
			if u.Input+u.CacheRead+u.CacheWrite+u.HiddenTokens != clientInput+700+300+hidden {
				t.Fatalf("total not preserved: %+v", u)
			}
			e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
			if e.Input != clientInput || e.CacheRead != 700 || e.CacheWrite != 300 || e.HiddenTokens != hidden {
				t.Fatalf("ledger carve wrong: %+v", e)
			}
			wantHidden := float64(hidden) * 3.0 / 1e6
			if math.Abs(e.HiddenChargedUSD-wantHidden) > 1e-12 {
				t.Fatalf("hidden not charged separately: %v want %v", e.HiddenChargedUSD, wantHidden)
			}
		})
	}
}

// 上报总量小于隐藏基线：隐藏取 min，客户端输入不得为负，总量守恒。
func TestReportedInputTinyRequestClampsHidden(t *testing.T) {
	h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = kiro.ReportedConservative })
	const reported = 100 // 远小于 hidden
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"outputTokens":2}}`, reported))...)
	}
	u := decodeWire(t, h.post(t, "/v1/messages", convo("hi"), nil))
	if u.HiddenTokens != reported || u.Input != 0 || u.CacheRead != 0 || u.CacheWrite != 0 {
		t.Fatalf("tiny carve: %+v want hidden=%d, client 0", u, reported)
	}
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.HiddenTokens != reported || e.Input != 0 {
		t.Fatalf("tiny ledger: %+v", e)
	}
}

// 配置覆盖隐藏基线、hidden_tokens_billing=off：隐藏仍分离计量，只是不计费。
func TestReportedInputHiddenOverrideAndBillingOff(t *testing.T) {
	const hidden = 4321
	h := newHarness(t, 1, func(c *config.Config) {
		c.ReportedUsage = kiro.ReportedConservative
		c.HiddenTokens = map[string]int{"claude-sonnet-4.5": hidden}
		c.HiddenTokensBilling = "off"
	})
	const clientInput = 2000
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"outputTokens":1}}`, clientInput+hidden))...)
	}
	u := decodeWire(t, h.post(t, "/v1/messages", convo("hi"), nil))
	if u.Input != clientInput || u.HiddenTokens != hidden {
		t.Fatalf("override carve: %+v want input=%d hidden=%d", u, clientInput, hidden)
	}
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.HiddenTokens != hidden || e.HiddenChargedUSD != 0 {
		t.Fatalf("billing off should keep hidden count but zero usd: %+v", e)
	}
}

// 流式：settled_usage 的独立隐藏账与 message_delta 一致，且不高于 message_start 估算。
func TestReportedInputStreamHiddenSettled(t *testing.T) {
	hidden := kiro.HiddenTokens("claude-sonnet-4.5")
	h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = kiro.ReportedConservative })
	const clientInput = 5000
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"ok"}`)
		return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"cacheReadInputTokens":1500,"outputTokens":2}}`, clientInput+hidden))...)
	}
	body := strings.Replace(convo("hi"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
	res := h.post(t, "/v1/messages", body, nil)
	defer res.Body.Close()
	var startHidden, endHidden int
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string
			Message struct {
				Usage wireUsage
			}
			Usage wireUsage
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "message_start":
			startHidden = ev.Message.Usage.HiddenTokens
		case "message_delta":
			endHidden = ev.Usage.HiddenTokens
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if endHidden != hidden || startHidden > endHidden {
		t.Fatalf("stream hidden not settled: start=%d end=%d want=%d", startHidden, endHidden, hidden)
	}
	e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
	if e.HiddenTokens != hidden || e.Input != clientInput || e.CacheRead != 1500 {
		t.Fatalf("stream ledger carve wrong: %+v", e)
	}
}

func TestPartialUsageEventsBilling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   usageBody
	}{
		{
			name: "output update preserves input",
			events: []string{
				`{"uncachedInputTokens":9052,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"outputTokens":11}`,
				`{"totalTokens":10011,"normalizedTokenUsage":0.5}`,
			},
			want: usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 11},
		},
		{
			name: "input update preserves output",
			events: []string{
				`{"outputTokens":9}`,
				`{"uncachedInputTokens":9052,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000}`,
			},
			want: usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 9},
		},
		{
			name: "cache update recomputes uncached input",
			events: []string{
				`{"inputTokens":14052,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"cacheReadInputTokens":4000}`,
			},
			want: usageBody{Input: 3000, CacheRead: 4000, CacheWrite: 3000, Output: 9},
		},
		{
			name: "explicit zero overrides previous values",
			events: []string{
				`{"uncachedInputTokens":9052,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"uncachedInputTokens":0,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":0}`,
			},
		},
	} {
		for _, mode := range []string{kiro.ReportedRaw, kiro.ReportedConservative, kiro.ReportedSum} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, mode, streaming), func(t *testing.T) {
					h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = mode })
					h.up.stream = func(string) []byte {
						b := frame("assistantResponseEvent", `{"content":"hello world"}`)
						for _, event := range tc.events {
							b = append(b, frame("metadataEvent", `{"tokenUsage":`+event+`}`)...)
						}
						return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
					}
					want := tc.want
					if mode == kiro.ReportedSum {
						// Sum consumes each event as a delta; missing fields contribute zero.
						want = usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 9}
						switch tc.name {
						case "output update preserves input":
							want.Output = 20
						case "cache update recomputes uncached input":
							want.CacheRead = 6000
						}
					}
					want.Credits = 0.5
					body := convo("hi")
					if streaming {
						body = strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1)
					}
					res := h.post(t, "/v1/messages", body, nil)
					var got usageBody
					if !streaming {
						got = decodeUsage(t, res)
					} else {
						var sawUsage bool
						sc := bufio.NewScanner(res.Body)
						for sc.Scan() {
							data, ok := strings.CutPrefix(sc.Text(), "data: ")
							if !ok || !strings.Contains(data, `"message_delta"`) {
								continue
							}
							var event struct {
								Settled usageBody `json:"settled_usage"`
							}
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							got, sawUsage = event.Settled, true
						}
						if err := sc.Err(); err != nil || !sawUsage {
							t.Fatalf("stream usage missing: %v", err)
						}
					}
					if got != want {
						t.Fatalf("response usage = %+v, want %+v", got, want)
					}
					e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
					cost := h.s.pricer.Cost(e.Model, turn.Usage{Input: e.Input, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Output: e.Output, Credits: e.Credits, KiroInput: e.HiddenTokens})
					if e.Input != want.Input || e.CacheRead != want.CacheRead || e.CacheWrite != want.CacheWrite || e.Output != want.Output || math.Abs(e.CostUSD-cost) > 1e-12 {
						t.Fatalf("journal usage differs from response: %+v, want cost %v", e, cost)
					}
				})
			}
		}
	}
}

func TestFailedAttemptCacheBillingAcrossRequests(t *testing.T) {
	for _, ttl := range []string{"5m", "1h"} {
		for _, stage := range []string{"before usage", "credits before content", "input before content", "after content"} {
			t.Run(ttl+"/"+stage, func(t *testing.T) {
				h := newHarness(t, 2, func(c *config.Config) { c.CacheMode = meter.ModeExplicit })
				var clock atomic.Int64
				clock.Store(time.Now().Unix())
				h.s.cache = meter.NewCache(func() time.Time { return time.Unix(clock.Load(), 0) })
				var failed atomic.Bool
				h.up.stream = func(string) []byte {
					if failed.CompareAndSwap(false, true) {
						var b []byte
						if stage == "after content" {
							b = frame("assistantResponseEvent", `{"content":"partial answer"}`)
						}
						if stage == "input before content" {
							b = frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":9052,"cacheWriteInputTokens":3000}}`)
							b = append(b, frame("metadataEvent", `{"tokenUsage":{"outputTokens":0}}`)...)
						}
						if stage != "before usage" {
							b = append(b, frame("meteringEvent", `{"unit":"credit","usage":0.25}`)...)
						}
						return append(b, frame("throttlingError", `{"message":"Rate limited"}`)...)
					}
					b := frame("assistantResponseEvent", `{"content":"hello world"}`)
					return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
				}
				body := cached(convo("fix the bug"), ttl)
				u1 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				v := waitRequests(t, h, usage.Query{}, 1)
				if u1.CacheRead != 0 || u1.CacheWrite == 0 {
					t.Fatalf("successful account inherited failed account's cache: %+v", u1)
				}
				if stage != "before usage" {
					if v.TotalEntries != 2 || v.Totals.Retried != 1 || v.Totals.RetryCredits != 0.25 || v.Entries[1].CostUSD != 0 {
						t.Fatalf("failed attempt cost missing or billed downstream: %+v, entries %+v", v.Totals, v.Entries)
					}
					if stage == "input before content" {
						e := v.Entries[1]
						wantLong := 0
						if ttl == "1h" {
							wantLong = 3000
						}
						if e.Input != 5000 || e.HiddenTokens != 4052 || e.HiddenChargedUSD != 0 || e.CacheWrite != 3000 || e.CacheWrite1h != wantLong || e.Output != 0 {
							t.Fatalf("partial reported input lost before failover: %+v", e)
						}
					}
				} else if v.TotalEntries != 1 || v.Totals.Retried != 0 {
					t.Fatalf("unused attempt billed: %+v", v)
				}
				if v.Totals.CostUSD != v.Entries[0].CostUSD {
					t.Fatalf("failed attempt changed downstream cost: %+v", v.Totals)
				}

				// Bring the failed account back and force the next request onto it.
				tokens := h.up.tokens()
				badID := "a" + strings.TrimPrefix(tokens[0], "tok")
				goodID := "a" + strings.TrimPrefix(tokens[1], "tok")
				if err := h.pool.SetDisabled(goodID, true, "test cache isolation"); err != nil {
					t.Fatal(err)
				}
				if err := h.pool.SetDisabled(badID, false, ""); err != nil {
					t.Fatal(err)
				}
				u2 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				if stage == "after content" || stage == "input before content" {
					if u2.CacheRead != u1.CacheWrite || u2.CacheWrite != 0 {
						t.Fatalf("processed prefix should be warm on failed account: %+v after %+v", u2, u1)
					}
				} else if u2 != u1 {
					t.Fatalf("failure without input usage warmed cache: %+v after %+v", u2, u1)
				}
				waitRequests(t, h, usage.Query{}, 2)

				clock.Add(int64((6 * time.Minute) / time.Second))
				u3 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				if ttl == "1h" {
					if u3.CacheRead != u1.CacheWrite || u3.CacheWrite != 0 {
						t.Fatalf("1h prefix expired early: %+v", u3)
					}
				} else if u3 != u1 {
					t.Fatalf("5m prefix still warm after expiry: %+v after %+v", u3, u1)
				}
				waitRequests(t, h, usage.Query{}, 3)
			})
		}
	}
}

func TestUsageBeforeFirstContentTerminalBilling(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", streaming), func(t *testing.T) {
			h := newHarness(t, 2, nil)
			h.up.stream = func(string) []byte {
				b := frame("meteringEvent", `{"unit":"credit","usage":0.25}`)
				return append(b, frame("validationError", `{"message":"Input is too long"}`)...)
			}
			body := cached(convo("hi"), "1h")
			if streaming {
				body = strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1)
			}
			res := h.post(t, "/v1/messages", body, nil)
			if _, err := io.Copy(io.Discard, res.Body); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusBadRequest || strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("terminal error started a reply: status %d, content-type %q", res.StatusCode, res.Header.Get("Content-Type"))
			}
			v := waitRequests(t, h, usage.Query{}, 1)
			e := v.Entries[0]
			if v.TotalEntries != 1 || e.Attempts != 1 || e.Retried || !e.Aborted || e.Credits != 0.25 || e.CostUSD != 0 || e.UpstreamUSD != h.s.pricer.CreditsUSD(0.25) || e.Input+e.CacheRead+e.CacheWrite+e.Output != 0 {
				t.Fatalf("terminal usage not settled: %+v", e)
			}
			if stats := h.pool.Totals(); stats.Credits != 0.25 || stats.Requests != 0 {
				t.Fatalf("terminal pool cost not recorded: %+v", stats)
			}
		})
	}
}
