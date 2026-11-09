// Package email provides a tiny pluggable interface for sending transactional
// email. The handler code talks to a single `Sender`; the concrete impl is
// chosen at startup based on config.
//
// Two implementations ship today:
//   - NoopSender: logs the message and returns nil. Used in dev when no
//     RESEND_API_KEY is set, and in all tests (no network).
//   - ResendSender: posts to https://api.resend.com/emails. Used in
//     production when both RESEND_API_KEY and EMAIL_FROM are set.
//
// To swap providers (SendGrid, Mailgun, Gmail SMTP) later, write one more
// file that implements Sender and select it from Pick() — no caller changes.
package email

import (
	"context"
	"log"

	"github.com/Pavelbotsu/sameway_backend/config"
)

type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Active is the package-level Sender chosen at startup. Handlers depend on
// this variable; tests override it before each test by direct assignment.
var Active Sender = &NoopSender{}

// Init picks a concrete Sender from config. Call once at startup from main.
// Falls back to NoopSender if Resend is not fully configured.
func Init(cfg config.Config) {
	if cfg.ResendAPIKey != "" && cfg.EmailFrom != "" {
		Active = NewResendSender(cfg)
		return
	}
	if cfg.IsProd() {
		log.Printf("email: RESEND_API_KEY/EMAIL_FROM not set — password-reset and verification emails will NOT be sent")
	}
	Active = &NoopSender{Redact: cfg.IsProd()}
}
