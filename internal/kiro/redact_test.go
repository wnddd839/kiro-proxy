package kiro

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// SafeLogger 只包一层（With / WithGroup 后也是）；成员 key_name 保留，凭证类字段、Bearer、
// 嵌在 error 里的完整请求正文、分组里的密钥都被脱敏。
func TestSafeLogger(t *testing.T) {
	var b bytes.Buffer
	log := SafeLogger(slog.New(slog.NewJSONHandler(&b, nil)))
	with := log.With("key_name", "member-alice", "access_token", "opaque-real-looking-secret").WithGroup("upstream")
	if SafeLogger(log) != log || SafeLogger(with) != with {
		t.Fatal("safe logger wrapped twice")
	}
	with.ErrorContext(context.Background(), "failed Bearer abcdefghijk",
		"key", "opaque-credential",
		"err", errors.New(`refused: {"conversationState":{"content":"private full request"}}`),
		"details", slog.GroupValue(slog.String("refreshToken", "opaque-refresh-secret"), slog.String("note", "api_key=opaque-api-secret")))
	out := b.String()
	if !strings.Contains(out, "member-alice") || !strings.Contains(out, "REDACTED") {
		t.Fatalf("key name or redaction marker missing: %s", out)
	}
	for _, secret := range []string{"opaque-real-looking-secret", "abcdefghijk", "opaque-credential", "private full request", "opaque-refresh-secret", "opaque-api-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaked %q: %s", secret, out)
		}
	}
}

// 上游正文进错误消息或调试快照前脱敏：不回显非结构化正文与凭证，保留分类所需的文字与对话正文。
func TestUpstreamBodiesAreRedacted(t *testing.T) {
	for _, raw := range []string{`<html>private full request</html>`, `{"conversationState":{"content":"private full request"}}`} {
		if got := errorMessage(503, []byte(raw)); strings.Contains(got, "private full request") {
			t.Fatalf("body exposed: %s", got)
		}
	}
	if got := errorMessage(429, []byte(`{"message":"suspicious activity; Bearer abcdefghijk"}`)); !strings.Contains(got, "suspicious activity") || strings.Contains(got, "abcdefghijk") {
		t.Fatalf("classification message lost or secret leaked: %s", got)
	}
	payload, err := RedactPayload([]byte(`{"source":{"type":"base64","data":"opaqueBase64ImageContent"},"token":"opaqueCredential","content":"safe text"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "opaqueBase64ImageContent") || strings.Contains(string(payload), "opaqueCredential") || !strings.Contains(string(payload), "safe text") {
		t.Fatalf("payload redaction failed: %s", payload)
	}
}
