package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// timeoutBody resets the idle watchdog on received bytes, not downstream pings.
type timeoutBody struct {
	io.ReadCloser
	mu     sync.Mutex
	timer  *time.Timer
	idle   time.Duration
	cancel context.CancelFunc
	closed bool
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if n > 0 && !b.closed {
		b.timer.Reset(b.idle)
	}
	b.mu.Unlock()
	return n, err
}

func (b *timeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

func durationOr(v time.Duration, fallback time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return fallback
}

func (s *Server) configureTransport() {
	h := *s.pool.Client().HTTP
	tr, ok := h.Transport.(*http.Transport)
	if !ok && h.Transport == nil {
		tr = http.DefaultTransport.(*http.Transport)
		ok = true
	}
	if !ok {
		return
	}
	tr = tr.Clone()
	d := durationOr(time.Duration(s.cfg.UpstreamConnectTimeout), 15*time.Second)
	tr.DialContext = (&net.Dialer{Timeout: d, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = d
	tr.ResponseHeaderTimeout = durationOr(time.Duration(s.cfg.UpstreamFirstByteTimeout), 60*time.Second)
	h.Transport = tr
	s.pool.Client().HTTP = &h
}
