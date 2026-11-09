package email

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestNoopSender_DevLogsBody(t *testing.T) {
	out := captureLog(t, func() {
		_ = NoopSender{}.Send(context.Background(), "a@x.com", "Reset", "code 123456")
	})
	if !strings.Contains(out, "123456") {
		t.Fatalf("dev mode should log the code for developers, got %q", out)
	}
}

// In production a reset code must never reach the logs.
func TestNoopSender_RedactedHidesBody(t *testing.T) {
	out := captureLog(t, func() {
		_ = NoopSender{Redact: true}.Send(context.Background(), "a@x.com", "Reset", "code 123456")
	})
	if strings.Contains(out, "123456") {
		t.Fatalf("redacted sender leaked the body: %q", out)
	}
	if !strings.Contains(out, "a@x.com") {
		t.Fatalf("recipient should still be logged for ops: %q", out)
	}
}
