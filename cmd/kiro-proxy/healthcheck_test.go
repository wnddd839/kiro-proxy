package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeHealth(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"healthy", http.StatusOK, `{"ok":true}`, true},
		{"journal down", http.StatusServiceUnavailable, `{"ok":false}`, false},
		{"not found", http.StatusNotFound, "nope", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			err := probeHealth(strings.TrimPrefix(srv.URL, "http://"))
			if tc.ok && err != nil {
				t.Fatalf("healthy endpoint returned %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("unhealthy endpoint returned nil")
			}
		})
	}
}

func TestProbeHealthUnreachable(t *testing.T) {
	// A closed port: bind then close so the address is almost certainly free.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()

	if err := probeHealth(addr); err == nil {
		t.Fatal("closed listener returned nil")
	}
}

func TestProbeHealthNormalizesWildcard(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, port, ok := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	if !ok {
		t.Fatalf("unexpected test URL %q", srv.URL)
	}
	if err := probeHealth("0.0.0.0:" + port); err != nil {
		t.Fatalf("wildcard listen should probe loopback: %v", err)
	}
	if got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
