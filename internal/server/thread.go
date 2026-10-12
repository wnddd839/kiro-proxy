package server

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/normalize"
	"kiro-proxy/internal/sessionpin"
)

type nativePin struct {
	Thinking *anthropic.Thinking     `json:"thinking,omitzero"`
	Output   *anthropic.OutputConfig `json:"output,omitzero"`
}

type systemPrefix struct {
	Length int    `json:"length"`
	Hash   string `json:"hash"`
	Thread string `json:"thread"`
}

func prefixHash(s string) string {
	v := sha256.Sum256([]byte(s))
	return hex.EncodeToString(v[:])
}

// Remember only a prefix digest and length, never the system text itself.
func (s *Server) stableThread(r *http.Request, req *anthropic.Request, k *keys.Key) (string, string) {
	msgs := normalize.PinMessages(req)
	client := cmp.Or(req.PromptCacheKey, r.Header.Get("X-Claude-Code-Session-Id"), normalize.MetadataSession(req))
	user := make([]map[string]any, 0, 1)
	for _, m := range msgs {
		if m["role"] == "user" {
			user = append(user, m)
		}
	}
	base := sessionpin.Key(r.Header, client, user) + "|" + sessionpin.Key(nil, "", user)
	if k != nil {
		base = "key:" + k.ID + "|" + base
	}
	system := req.System.Text("\n")
	s.threadPrefixes.mu.Lock()
	defer s.threadPrefixes.mu.Unlock()
	old, ok := s.threadPrefixes.m[base]
	reason := ""
	if ok && time.Now().Before(old.until) && len(system) >= old.v.Length && prefixHash(system[:old.v.Length]) == old.v.Hash {
		s.threadPrefixes.setLocked(base, old.v)
		return old.v.Thread, ""
	}
	if ok {
		reason = "system prefix changed"
	}
	thread := threadKey(r, req, k)
	s.threadPrefixes.setLocked(base, systemPrefix{Length: len(system), Hash: prefixHash(system), Thread: thread})
	return thread, reason
}
