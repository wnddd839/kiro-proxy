package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

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
