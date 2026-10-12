package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

var secrets = regexp.MustCompile(`(?i)(bearer\s+[^\s"']{8,}|(?:sk-|ksk_|AKIA)[A-Za-z0-9_-]{8,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|(?:access[_-]?token|refresh[_-]?token|api[_-]?key|client[_-]?secret|password)["']?\s*[:=]\s*["']?[^\s"',;]+)`)
var diagnosticBody = regexp.MustCompile(`\{|<|\[\s*["{]`)

// Redact removes credential-like strings before diagnostics or logs are written.
func Redact(s string) string { return secrets.ReplaceAllString(s, "[REDACTED]") }

// Diagnostic strips structured bodies and bounds untrusted diagnostic text.
// Raw upstream bytes remain available privately for failure classification.
func Diagnostic(s string) string {
	s = Redact(s)
	if loc := diagnosticBody.FindStringIndex(s); loc != nil {
		i := loc[0]
		s = s[:i] + "[body omitted]"
	}
	r := []rune(s)
	if len(r) > 240 {
		s = string(r[:240]) + "..."
	}
	return s
}

func secretField(key string) bool {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch k {
	case "authorization", "token", "accesstoken", "refreshtoken", "apikey", "key", "clientsecret", "password", "profilearn", "bytes", "body", "payload", "requestbody", "raw":
		return true
	}
	return false
}

// SafeLogger applies redaction even to With attributes, groups and error values.
func SafeLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		log = slog.Default()
	}
	if _, ok := log.Handler().(safeHandler); ok {
		return log
	}
	return slog.New(safeHandler{log.Handler()})
}

type safeHandler struct{ next slog.Handler }

func (h safeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h safeHandler) Handle(ctx context.Context, record slog.Record) error {
	r := slog.NewRecord(record.Time, record.Level, Diagnostic(record.Message), record.PC)
	record.Attrs(func(a slog.Attr) bool { r.AddAttrs(safeAttr(a)); return true })
	return h.next.Handle(ctx, r)
}
func (h safeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = safeAttr(a)
	}
	return safeHandler{h.next.WithAttrs(out)}
}
func (h safeHandler) WithGroup(name string) slog.Handler {
	return safeHandler{h.next.WithGroup(Diagnostic(name))}
}
func safeAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	if secretField(a.Key) {
		a.Value = slog.StringValue("[REDACTED]")
		return a
	}
	switch a.Value.Kind() {
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, v := range group {
			out[i] = safeAttr(v)
		}
		a.Value = slog.GroupValue(out...)
	case slog.KindString:
		a.Value = slog.StringValue(Diagnostic(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && !errors.Is(err, nil) {
			a.Value = slog.StringValue(Diagnostic(fmt.Sprint(err)))
		} else {
			a.Value = slog.StringValue("[value omitted]")
		}
	}
	return a
}

// RedactPayload keeps message structure for prefix comparison, not authorization,
// profile identifiers, image bytes or explicitly named secrets.
func RedactPayload(payload []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				key := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
				switch key {
				case "authorization", "token", "accesstoken", "refreshtoken", "apikey", "key", "clientsecret", "password", "profilearn", "bytes", "data":
					x[k] = "[REDACTED]"
				default:
					x[k] = walk(v)
				}
			}
		case []any:
			for i, v := range x {
				x[i] = walk(v)
			}
		case string:
			return Redact(x)
		}
		return v
	}
	return json.Marshal(walk(value))
}
