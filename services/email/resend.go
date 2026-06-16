package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
)

// ResendSender posts emails to https://api.resend.com/emails. Free tier
// covers 3,000 messages/month, which is plenty for password resets in
// testing.
//
// The struct is tiny on purpose: the caller passes the recipient + content,
// we never store templates or HTML wrappers here. A more elaborate transport
// (templating, tracking, headers) belongs in a higher layer.
type ResendSender struct {
	httpClient *http.Client
	apiKey     string
	from       string
}

func NewResendSender(cfg config.Config) *ResendSender {
	return &ResendSender{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		apiKey:     cfg.ResendAPIKey,
		from:       cfg.EmailFrom,
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

func (r *ResendSender) Send(ctx context.Context, to, subject, body string) error {
	payload, err := json.Marshal(resendRequest{
		From:    r.from,
		To:      []string{to},
		Subject: subject,
		Text:    body,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("resend http %d", resp.StatusCode)
	}
	return nil
}
