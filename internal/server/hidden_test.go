package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

// 8 轮持续增长的对话：隐藏上下文（4052）不进客户端 input，在线上与账本独立记录；
// 跟随前缀缓存计价——命中轮按读价，写入轮按写价，关闭模拟时按输入价。
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
			pr, _ := h.s.pricer.PriceOf("claude-sonnet-4.5")
			sessionHidden := 0.0
			for round := 1; round <= 8; round++ {
				target := 1700 + (round-1)*10800/7
				// Binary-search real local tokens; the simulator still sees real growing message history.
				lo, hi := 0, target*3
				req := anthropic.Request{Model: "claude-sonnet-4.5", MaxTokens: 1000}
				for lo < hi {
					n := (lo + hi) / 2
					req.Messages = append(append([]anthropic.Message{}, messages...), anthropic.Message{Role: "user", Content: anthropic.Content{{Type: "text", Text: strings.Repeat(" a", n)}}})
					if meter.Analyze(&req, "", 0, meter.ModeOff).Tokens < target {
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
				u := decodeWire(t, h.post(t, "/v1/messages", string(body), map[string]string{"X-Session-Id": "eight-rounds"}))
				e := waitRequests(t, h, usage.Query{}, int64(round)).Entries[0]
				if u.Input+u.CacheRead+u.CacheWrite != prompt || u.Output != output || u.HiddenTokens != 4052 || e.HiddenTokens != 4052 {
					t.Fatalf("round %d: usage=%+v journal=%+v prompt=%d", round, u, e, prompt)
				}
				if round == 1 && scenario == "hit" {
					// 管理台与导出依赖的账本字段名
					raw, _ := json.Marshal(e)
					for _, key := range []string{`"hidden_tokens":4052`, `"hidden_cache_write":4052`, `"hidden_tokens_charged_usd":`} {
						if !strings.Contains(string(raw), key) {
							t.Fatalf("ledger row lacks %s: %s", key, raw)
						}
					}
				}
				wantPrice := pr.Input
				switch {
				case scenario == "hit" && round > 1:
					wantPrice = pr.CacheRead
				case scenario != "miss":
					wantPrice = pr.CacheWrite
				}
				if want := 4052 * wantPrice / 1e6; math.Abs(e.HiddenChargedUSD-want) > 1e-12 {
					t.Fatalf("round %d: hidden charged %v, want %v (4052 at %v/M)", round, e.HiddenChargedUSD, want, wantPrice)
				}
				sessionHidden += e.HiddenChargedUSD
				if scenario == "hit" && round > 1 && (u.CacheRead != previous || u.CacheWrite <= 0) {
					t.Fatalf("prefix not reused: %+v previous=%d", u, previous)
				}
				if scenario == "cold" && (u.CacheRead != 0 || u.CacheWrite != prompt) {
					t.Fatalf("cold split: %+v", u)
				}
				if scenario == "miss" && (u.Input != prompt || u.CacheRead != 0 || u.CacheWrite != 0) {
					t.Fatalf("miss split: %+v", u)
				}
				t.Logf("round=%d prompt=%d input=%d read=%d write=%d output=%d hidden=%d hidden_usd=%.6f", round, prompt, u.Input, u.CacheRead, u.CacheWrite, u.Output, e.HiddenTokens, e.HiddenChargedUSD)
				previous = prompt
				messages = append(req.Messages, anthropic.Message{Role: "assistant", Content: anthropic.Content{{Type: "text", Text: answer}}})
			}
			// 热会话 8 轮隐藏费用 = 1 次写入 + 7 次读，远低于每轮按普通输入
			if old := 8 * 4052 * pr.Input / 1e6; scenario == "hit" && sessionHidden > old/3 {
				t.Fatalf("warm session hidden %.6f not discounted vs %.6f", sessionHidden, old)
			}
			t.Logf("%s: 8-round hidden USD %.6f (flat input price would be %.6f)", scenario, sessionHidden, 8*4052*pr.Input/1e6)
		})
	}
}

// hidden_tokens 覆盖与 hidden_tokens_billing=off：本地计量与上游上报两条路径都独立记录隐藏数、不收其费用；
// 模型表如实标注校准来源。
func TestHiddenOverrideAndBillingOff(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(fmt.Sprintf("reported=%v", reported), func(t *testing.T) {
			h := newHarness(t, 1, func(c *config.Config) {
				c.HiddenTokensBilling = "off"
				c.HiddenTokens = map[string]int{"claude-sonnet-4.5": 4321}
			})
			wantInput := 0 // bigSystem 首轮整段写入缓存
			h.up.stream = func(string) []byte { return frame("assistantResponseEvent", `{"content":"ok"}`) }
			if reported {
				wantInput = 2000
				h.up.stream = func(string) []byte {
					b := frame("assistantResponseEvent", `{"content":"ok"}`)
					return append(b, frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":6321,"outputTokens":1}}`)...)
				}
			}
			u := decodeWire(t, h.post(t, "/v1/messages", convo("hi"), nil))
			e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
			if u.Input != wantInput || u.HiddenTokens != 4321 || e.HiddenTokens != 4321 || e.HiddenChargedUSD != 0 {
				t.Fatalf("override/off: wire=%+v entry=%+v", u, e)
			}
			if e.CostUSD != h.s.pricer.APICost(e.Model, turn.Usage{Input: e.Input, CacheRead: e.CacheRead, CacheWrite: e.CacheWrite, Output: e.Output, KiroInput: e.HiddenTokens}) {
				t.Fatalf("off still billed: %+v", e)
			}
		})
	}
	h := newHarness(t, 1, nil)
	h.s.models.merge([]kiro.Model{{ID: "claude-opus-5.5"}, {ID: "deepseek-3.2"}})
	for _, r := range h.s.modelRows() {
		if r.ID == "claude-opus-5.5" && (r.HiddenCalibration != "未校准" || r.HiddenTokens != 4052) {
			t.Fatalf("invented calibration: %+v", r)
		}
		if r.ID == "deepseek-3.2" && (r.HiddenCalibration != "已校准" || r.HiddenTokens != 3699) {
			t.Fatalf("measured model not used: %+v", r)
		}
	}
}

// 上游报了输入侧 tokenUsage：报告总量含隐藏上下文，按 未命中 → 写入 → 读 切出至多一份基线；
// 客户端各项不为负、总量守恒，切出部分按来源类别计价，整笔费用等于报告按类计价。
// 流式：message_start 为零，message_delta 与 settled_usage 都是最终结算值。
func TestReportedInputCarvesHidden(t *testing.T) {
	type report struct{ uncached, read, write, output int }
	for _, c := range []struct {
		name                    string
		up                      report
		want                    wireUsage
		hiddenRead, hiddenWrite int // 期望的 HiddenCacheRead / HiddenCacheWrite
	}{
		{name: "carve from uncached", up: report{1234 + 4052, 700, 300, 5},
			want: wireUsage{usageBody: usageBody{Input: 1234, CacheRead: 700, CacheWrite: 300, Output: 5}, HiddenTokens: 4052}},
		{name: "tiny report clamps hidden", up: report{100, 0, 0, 2},
			want: wireUsage{usageBody: usageBody{Output: 2}, HiddenTokens: 100}},
		{name: "spill into write then read", up: report{1000, 5000, 3000, 2},
			want:       wireUsage{usageBody: usageBody{CacheRead: 4948, Output: 2}, HiddenTokens: 4052},
			hiddenRead: 52, hiddenWrite: 3000},
		{name: "explicit zero report", up: report{0, 0, 0, 1},
			want: wireUsage{usageBody: usageBody{Output: 1}}},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", c.name, streaming), func(t *testing.T) {
				h := newHarness(t, 1, func(cfg *config.Config) { cfg.ReportedUsage = kiro.ReportedConservative })
				h.up.stream = func(string) []byte {
					b := frame("assistantResponseEvent", `{"content":"ok"}`)
					return append(b, frame("metadataEvent", fmt.Sprintf(`{"tokenUsage":{"uncachedInputTokens":%d,"cacheReadInputTokens":%d,"cacheWriteInputTokens":%d,"outputTokens":%d}}`,
						c.up.uncached, c.up.read, c.up.write, c.up.output))...)
				}
				body := convo("hi")
				var got wireUsage
				if streaming {
					res := h.post(t, "/v1/messages", strings.Replace(body, `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1), nil)
					start, end, settled := streamUsage(t, res)
					if start != (wireUsage{}) || end != settled {
						t.Fatalf("stream usage: start=%+v end=%+v settled=%+v", start, end, settled)
					}
					got = end
				} else {
					got = decodeWire(t, h.post(t, "/v1/messages", body, nil))
				}
				got.Credits = 0
				if got != c.want {
					t.Fatalf("wire usage %+v, want %+v", got, c.want)
				}
				e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
				if e.Input != c.want.Input || e.CacheRead != c.want.CacheRead || e.CacheWrite != c.want.CacheWrite || e.HiddenTokens != c.want.HiddenTokens ||
					e.HiddenCacheRead != c.hiddenRead || e.HiddenCacheWrite != c.hiddenWrite {
					t.Fatalf("ledger carve: %+v", e)
				}
				pr, _ := h.s.pricer.PriceOf("claude-sonnet-4.5")
				carved := c.want.HiddenTokens - c.hiddenRead - c.hiddenWrite
				wantHidden := (float64(carved)*pr.Input + float64(c.hiddenRead)*pr.CacheRead + float64(c.hiddenWrite)*pr.CacheWrite) / 1e6
				wantCost := (float64(c.up.uncached)*pr.Input + float64(c.up.read)*pr.CacheRead + float64(c.up.write)*pr.CacheWrite + float64(c.up.output)*pr.Output) / 1e6
				if math.Abs(e.HiddenChargedUSD-wantHidden) > 1e-12 || math.Abs(e.CostUSD-wantCost) > 1e-12 {
					t.Fatalf("hidden=%v want %v, cost=%v want %v (report priced per category)", e.HiddenChargedUSD, wantHidden, e.CostUSD, wantCost)
				}
			})
		}
	}
}

// 首条内容前就报错：只按已报的用量入账，同样切出隐藏部分。
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

// 未知模型：默认按已知最高各项单价计并告警；reject 在生成前拒绝。
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
			if known || pr.Warning == "" || pr.Input < 15 || pr.Output < 75 || e.HiddenChargedUSD != float64(4052)*pr.CacheWrite/1e6 {
				t.Fatalf("unknown model underpriced or unwarned: price=%+v entry=%+v", pr, e)
			}
		})
	}
}

// streamUsage 读完一条 Anthropic SSE，返回 message_start、message_delta 的 usage 与 settled_usage。
func streamUsage(t *testing.T, res *http.Response) (start, end, settled wireUsage) {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
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
			Settled wireUsage `json:"settled_usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case "message_start":
			start = ev.Message.Usage
		case "message_delta":
			end, settled = ev.Usage, ev.Settled
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return start, end, settled
}
