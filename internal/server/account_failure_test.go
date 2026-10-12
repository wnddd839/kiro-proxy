package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"kiro-proxy/internal/pool"
)

func TestOnlySuspendedAccountIsDisabledAndStaysDisabled(t *testing.T) {
	h := newHarness(t, 1, nil)
	h.up.reply = func(string) (int, string) { return 403, `{"message":"Your account is TemporarilySuspended"}` }
	first := h.post(t, "/v1/messages", convo("hi"), nil)
	io.Copy(io.Discard, first.Body)
	if first.StatusCode != 403 {
		t.Fatalf("upstream suspension status=%d", first.StatusCode)
	}
	v, _ := h.pool.Get("a0")
	if !v.Disabled || !strings.Contains(v.Note, "Suspended") {
		t.Fatalf("permanent failure shown as temporary cooling: %+v", v)
	}
	restored, err := pool.Open(h.poolPath, pool.Options{Client: h.pool.Client()})
	if err != nil {
		t.Fatal(err)
	}
	h.s.pool = restored
	res := h.post(t, "/v1/messages", convo("hi"), nil)
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusServiceUnavailable || len(h.up.tokens()) != 1 {
		t.Fatalf("disabled account retried after restart: status=%d calls=%v", res.StatusCode, h.up.tokens())
	}
}
