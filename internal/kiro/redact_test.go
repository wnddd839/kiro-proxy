package kiro

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSafeLoggerIsIdempotentAndPreservesKeyNames(t *testing.T) {
	var b bytes.Buffer
	log := SafeLogger(slog.New(slog.NewJSONHandler(&b, nil)))
	if SafeLogger(log) != log {
		t.Fatal("safe logger wrapped twice")
	}
	with := log.With("key_name", "member-alice").WithGroup("request")
	if SafeLogger(with) != with {
		t.Fatal("logger with attributes wrapped twice")
	}
	with.Info("generate", "key", "opaque-credential", "access_token", "opaque-token")
	if !strings.Contains(b.String(), "member-alice") || strings.Contains(b.String(), "opaque-credential") || strings.Contains(b.String(), "opaque-token") {
		t.Fatalf("key name missing or credential exposed: %s", b.String())
	}
}

func TestSafeLoggerSanitizesAllBoundaries(t *testing.T) {
	var b bytes.Buffer
	log := SafeLogger(slog.New(slog.NewJSONHandler(&b, nil)))
	log.With("access_token", "opaque-real-looking-secret").WithGroup("upstream").ErrorContext(context.Background(),
		"failed Bearer abcdefghijk", "err", errors.New(`refused: {"conversationState":{"content":"private full request"}}`),
		"details", slog.GroupValue(slog.String("refreshToken", "opaque-refresh-secret"), slog.String("note", "api_key=opaque-api-secret")))
	for _, secret := range []string{"opaque-real-looking-secret", "abcdefghijk", "private full request", "opaque-refresh-secret", "opaque-api-secret"} {
		if strings.Contains(b.String(), secret) {
			t.Fatalf("log leaked %q: %s", secret, b.String())
		}
	}
	if !strings.Contains(b.String(), "REDACTED") {
		t.Fatal("missing redaction marker")
	}
}

func TestErrorMessageDoesNotExposeUnstructuredBody(t *testing.T) {
	for _, raw := range []string{`<html>private full request</html>`, `{"conversationState":{"content":"private full request"}}`} {
		if got := errorMessage(503, []byte(raw)); strings.Contains(got, "private full request") {
			t.Fatalf("body exposed: %s", got)
		}
	}
	got := errorMessage(429, []byte(`{"message":"suspicious activity; Bearer abcdefghijk"}`))
	if !strings.Contains(got, "suspicious activity") || strings.Contains(got, "abcdefghijk") {
		t.Fatalf("classification message lost or secret leaked: %s", got)
	}
}

func TestRedactPayloadDropsInboundImageAndToken(t *testing.T) {
	raw, err := RedactPayload([]byte(`{"source":{"type":"base64","data":"opaqueBase64ImageContent"},"token":"opaqueCredential","content":"safe text"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "opaqueBase64ImageContent") || strings.Contains(string(raw), "opaqueCredential") || !strings.Contains(string(raw), "safe text") {
		t.Fatalf("payload redaction failed: %s", raw)
	}
}
