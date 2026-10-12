package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kiro-proxy/internal/keys"
)

func TestDeleteLastKeyKeepsAuthentication(t *testing.T) {
	h := newHarness(t, 1, nil)
	path := filepath.Join(t.TempDir(), "keys.json")
	st, err := keys.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.s.keys = st
	res := h.post(t, "/admin/keys", `{"name":"only member"}`, nil)
	var k keys.Key
	if err := json.NewDecoder(res.Body).Decode(&k); err != nil {
		t.Fatal(err)
	}
	for _, restart := range []bool{false, true} {
		if !restart {
			req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/admin/keys/"+k.ID, nil)
			deleted, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			deleted.Body.Close()
			if deleted.StatusCode != http.StatusNoContent {
				t.Fatalf("delete: %d", deleted.StatusCode)
			}
		} else {
			h.s.keys, err = keys.Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		rejected := h.post(t, "/v1/messages", convo("hi"), nil)
		rejected.Body.Close()
		if rejected.StatusCode != http.StatusUnauthorized {
			t.Fatalf("restart=%v: anonymous status=%d", restart, rejected.StatusCode)
		}
		listing, err := http.Get(h.srv.URL + "/admin/keys")
		if err != nil {
			t.Fatal(err)
		}
		var view struct{ Open bool }
		if err := json.NewDecoder(listing.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		listing.Body.Close()
		if view.Open {
			t.Fatal("console claims authentication is open")
		}
	}
	// Adding a replacement key restores authenticated access.
	replacement := h.post(t, "/admin/keys", `{"name":"replacement"}`, nil)
	if err := json.NewDecoder(replacement.Body).Decode(&k); err != nil {
		t.Fatal(err)
	}
	accepted := h.post(t, "/v1/messages", convo("hi"), map[string]string{"x-api-key": k.Secret})
	io.Copy(io.Discard, accepted.Body)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("replacement status=%d", accepted.StatusCode)
	}
}

func TestInvalidKeyFilesFailToLoad(t *testing.T) {
	for _, raw := range []string{
		`{"keys":[null]}`,
		`{"keys":[{"secret_sha256":"broken"}]}`,
		`{"keys":[{"secret_sha256":"` + strings.Repeat("z", 64) + `"}]}`,
		`{"keys":[{"secret":"same"},{"secret":"same"}]}`,
	} {
		path := filepath.Join(t.TempDir(), "keys.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := keys.Open(path, nil); err == nil {
			t.Fatalf("invalid credentials silently accepted: %s", raw)
		}
	}
}
