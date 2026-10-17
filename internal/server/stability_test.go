package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnddd839/kiro.cache/internal/config"
	"github.com/wnddd839/kiro.cache/internal/keys"
	"github.com/wnddd839/kiro.cache/internal/meter"
	"github.com/wnddd839/kiro.cache/internal/usage"
)

func TestLocalOutputLimitsHTTP(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, scenario := range []string{"max", "stop"} {
			t.Run(fmt.Sprintf("%s/stream=%v", scenario, streaming), func(t *testing.T) {
				h := newHarness(t, 1, nil)
				h.up.stream = func(string) []byte {
					b := frame("assistantResponseEvent", `{"content":"hello ST"}`)
					b = append(b, frame("assistantResponseEvent", `{"content":"OP secret excess output"}`)...)
					return append(b, frame("metadataEvent", `{"tokenUsage":{"outputTokens":1000}}`)...)
				}
				maxTokens, stop := 2, ""
				if scenario == "stop" {
					maxTokens = 100
					stop = `,"stop_sequences":["STOP"]`
				}
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","max_tokens":%d,"stream":%v%s,"messages":[{"role":"user","content":"hi"}]}`, maxTokens, streaming, stop)
				res := h.post(t, "/v1/messages", body, nil)
				defer res.Body.Close()
				raw, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "secret excess") || strings.Contains(string(raw), `"output_tokens":1000`) {
					t.Fatalf("excess output delivered or billed: %s", raw)
				}
				want := `"stop_reason":"max_tokens"`
				if scenario == "stop" {
					want = `"stop_reason":"stop_sequence"`
				}
				if !strings.Contains(string(raw), want) {
					t.Fatalf("wrong stop reason: %s", raw)
				}
				e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
				if e.Output > maxTokens {
					t.Fatalf("excess tokens charged: %+v", e)
				}
			})
		}
	}
}

func TestSystemAppendKeepsThread(t *testing.T) {
	h := newHarness(t, 1, nil)
	for _, sys := range []string{"stable prefix", "stable prefix\nnew suffix"} {
		body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","system":%q,"messages":[{"role":"user","content":"same first message"}]}`, sys)
		res := h.post(t, "/v1/messages", body, map[string]string{"X-Session-Id": "stable"})
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	ids := h.up.convIDs()
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("system append changed thread: %v", ids)
	}
}

// 客户端没给输出上限（Codex / Cursor 的 Chat、Responses 请求通常不带）时不得在本地截断长回复。
func TestOmittedMaxTokensDoesNotTruncate(t *testing.T) {
	var words []string
	for i := range 6000 {
		words = append(words, fmt.Sprintf("word%d", i))
	}
	answer := strings.Join(words, " ") + " TAILMARK"
	if n := meter.Count(answer); n <= 2*4096 {
		t.Fatalf("fixture too short: %d tokens", n)
	}
	bodies := map[string]string{
		"/v1/messages":         `{"model":"claude-sonnet-4.5","stream":%v,"messages":[{"role":"user","content":"write a lot"}]}`,
		"/v1/chat/completions": `{"model":"claude-sonnet-4.5","stream":%v,"messages":[{"role":"user","content":"write a lot"}]}`,
		"/v1/responses":        `{"model":"claude-sonnet-4.5","stream":%v,"input":"write a lot"}`,
	}
	for path, body := range bodies {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, streaming), func(t *testing.T) {
				h := newHarness(t, 1, nil)
				h.up.stream = func(string) []byte {
					var b []byte
					for i := 0; i < len(answer); i += 400 {
						b = append(b, frame("assistantResponseEvent", fmt.Sprintf(`{"content":%q}`, answer[i:min(i+400, len(answer))]))...)
					}
					return b
				}
				res := h.post(t, path, fmt.Sprintf(body, streaming), nil)
				defer res.Body.Close()
				raw, _ := io.ReadAll(res.Body)
				if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "TAILMARK") {
					t.Fatalf("long reply truncated without a client limit: status=%d tail=%q", res.StatusCode, raw[max(0, len(raw)-300):])
				}
				for _, cut := range []string{`"stop_reason":"max_tokens"`, `"finish_reason":"length"`, `"incomplete"`} {
					if strings.Contains(string(raw), cut) {
						t.Fatalf("reported as truncated (%s)", cut)
					}
				}
			})
		}
	}
}

func TestStopPrefixFlushesBeforeToolBlock(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.stream = func(string) []byte {
		b := frame("assistantResponseEvent", `{"content":"hello ST"}`)
		b = append(b, frame("toolUseEvent", `{"toolUseId":"tool-1","name":"read","input":{}}`)...)
		return b
	}
	body := `{"model":"claude-sonnet-4.5","max_tokens":100,"stream":true,"stop_sequences":["STOP"],"messages":[{"role":"user","content":"hi"}]}`
	res := h.post(t, "/v1/messages", body, nil)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	textAt, toolAt := strings.Index(string(raw), `"text":"ST"`), strings.Index(string(raw), `"type":"tool_use"`)
	if textAt < 0 || toolAt < 0 || textAt > toolAt {
		t.Fatalf("buffered stop prefix reordered after tool block: %s", raw)
	}
}

func TestUpstreamTimeoutsReleaseResources(t *testing.T) {
	for _, stage := range []string{"headers", "firstbyte", "idle"} {
		t.Run(stage, func(t *testing.T) {
			h := newHarness(t, 1, func(c *config.Config) {
				c.UpstreamFirstByteTimeout = config.Duration(80 * time.Millisecond)
				c.UpstreamIdleTimeout = config.Duration(80 * time.Millisecond)
				if stage == "firstbyte" {
					c.UpstreamIdleTimeout = config.Duration(3 * time.Second)
				}
				c.MaxAttempts = 1
			})
			k, err := h.s.keys.Create(keys.Key{Name: "timeout", LimitUSD: 10})
			if err != nil {
				t.Fatal(err)
			}
			h.up.streamWriter = func(w http.ResponseWriter, r *http.Request) {
				if stage == "firstbyte" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				if stage == "idle" {
					w.Write(frame("assistantResponseEvent", `{"content":"partial"}`))
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}
			start := time.Now()
			body := strings.Replace(convo("hi"), `"max_tokens":100`, `"max_tokens":100,"stream":true`, 1)
			res := h.post(t, "/v1/messages", body, map[string]string{"x-api-key": k.Secret})
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if time.Since(start) > 2*time.Second {
				t.Fatal("timeout did not terminate")
			}
			e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
			if e.Status == 200 || h.s.holds.reserved(k.ID).Requests != 0 {
				t.Fatalf("timeout leaked or settled success: %+v", e)
			}
			v, _ := h.pool.Get("a0")
			if v.InFlight != 0 {
				t.Fatalf("lease leaked: %+v", v)
			}
			if stage == "idle" && (e.Output == 0 || !e.Aborted || e.CostUSD <= 0) {
				t.Fatalf("partial delivery lost: %+v", e)
			}
			if stage != "idle" && e.CostUSD != 0 {
				t.Fatalf("undelivered failure charged: %+v", e)
			}
		})
	}
}

// 工具参数 JSON 损坏或被本地上限截断时明确报错，不改写成 {}；合法的空参数仍是 {}。
func TestToolArgumentsAreNotSilentlyReplaced(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, scenario := range []string{"broken", "next-tool", "limited", "valid", "empty"} {
			t.Run(fmt.Sprintf("%s/stream=%v", scenario, streaming), func(t *testing.T) {
				h := newHarness(t, 1, nil)
				h.up.stream = func(string) []byte {
					args := `{"toolUseId":"t1","name":"read","input":"{\"path\":\""}`
					if scenario == "valid" || scenario == "limited" {
						args = `{"toolUseId":"t1","name":"read","input":"{\"path\":\"a-very-long-file-name.txt\"}"}`
					}
					if scenario == "empty" {
						args = `{"toolUseId":"t1","name":"read","input":{}}`
					}
					b := frame("toolUseEvent", args)
					if scenario == "next-tool" {
						b = append(b, frame("toolUseEvent", `{"toolUseId":"t2","name":"read","input":{}}`)...)
					}
					return b
				}
				maxTokens := 100
				if scenario == "limited" {
					maxTokens = 12
				}
				body := fmt.Sprintf(`{"model":"claude-sonnet-4.5","stream":%v,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, streaming, maxTokens)
				res := h.post(t, "/v1/messages", body, nil)
				raw, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				good := scenario == "valid" || scenario == "empty"
				if good {
					if res.StatusCode != 200 || strings.Contains(string(raw), `"type":"error"`) {
						t.Fatalf("valid arguments rejected: %d %s", res.StatusCode, raw)
					}
					return
				}
				if !strings.Contains(string(raw), "tool arguments are not valid JSON") {
					t.Fatalf("invalid arguments hidden: %d %s", res.StatusCode, raw)
				}
				if streaming {
					if strings.Contains(string(raw), "event: message_stop") {
						t.Fatalf("broken tool reported success: %s", raw)
					}
				} else if res.StatusCode != http.StatusBadGateway || strings.Contains(string(raw), `"stop_reason":"tool_use"`) {
					t.Fatalf("broken tool reported success: %d %s", res.StatusCode, raw)
				}
			})
		}
	}
}
