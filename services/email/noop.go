package email

import (
	"context"
	"log"
)

// NoopSender prints the message to the application log and returns nil. Used
// in development (no API key set) and in tests (no network).
//
// The "EMAIL TO ..." log line is intentional: developers read it to grab a
// password-reset code without standing up an SMTP server.
type NoopSender struct{}

func (NoopSender) Send(_ context.Context, to, subject, body string) error {
	log.Printf("EMAIL TO %s | subject=%q | body=%q", to, subject, body)
	return nil
}
