package email

import (
	"context"
	"log"
)

// NoopSender logs the message instead of sending it. Used in development (no
// API key set) and in tests (no network).
//
// In development the full "EMAIL TO ..." line is intentional: developers read
// it to grab a password-reset code without an SMTP server. With Redact set
// (production without an email provider configured) only the recipient and
// subject are logged: the body holds a live reset/verification code, and log
// files are read by more people and systems than the account owner.
type NoopSender struct {
	Redact bool
}

func (s NoopSender) Send(_ context.Context, to, subject, body string) error {
	if s.Redact {
		log.Printf("EMAIL NOT SENT (no email provider configured) to %s | subject=%q | body redacted", to, subject)
		return nil
	}
	log.Printf("EMAIL TO %s | subject=%q | body=%q", to, subject, body)
	return nil
}
