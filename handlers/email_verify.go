package handlers

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services/email"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Phase 3.3: email verification. Mirrors the password-reset shape so the
// existing Resend infra is reused without modification — same 6-digit code,
// same 15-minute TTL, same 60-second resend cooldown.

const (
	emailVerifyTTL            = 15 * time.Minute
	emailVerifyResendCooldown = 60 * time.Second
)

// RequestEmailVerify issues (or resends) a 6-digit verification code to the
// authenticated user's email. The cooldown prevents an attacker who got hold
// of a session from flooding the user's inbox.
func RequestEmailVerify(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var emailAddr string
	var alreadyVerified *time.Time
	if err := db.DB.QueryRow(ctx,
		`SELECT email, email_verified_at FROM users WHERE id = $1`, userID,
	).Scan(&emailAddr, &alreadyVerified); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if alreadyVerified != nil {
		c.JSON(http.StatusOK, gin.H{"already_verified": true})
		return
	}

	// Cooldown check against any in-flight code.
	var existingCreatedAt time.Time
	err := db.DB.QueryRow(ctx,
		`SELECT created_at FROM email_verifications WHERE user_id = $1`, userID,
	).Scan(&existingCreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if err == nil && time.Since(existingCreatedAt) < emailVerifyResendCooldown {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":            "please wait before requesting another code",
			"retry_after_secs": int((emailVerifyResendCooldown - time.Since(existingCreatedAt)).Seconds()),
		})
		return
	}

	code, err := generateResetCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "code gen failed"})
		return
	}
	expiresAt := time.Now().Add(emailVerifyTTL)
	_, err = db.DB.Exec(ctx, `
		INSERT INTO email_verifications (user_id, code, expires_at, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			code = EXCLUDED.code,
			expires_at = EXCLUDED.expires_at,
			created_at = NOW(),
			-- Same reset as password_resets: a fresh code must come with a
			-- fresh attempt budget, or the cap locks the user out permanently.
			attempts = 0
	`, userID, code, expiresAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	subject := "Confirm your Sameway email"
	body := "Your verification code is " + code +
		".\n\nIt expires in 15 minutes. If you didn't request this, ignore this email."
	if sendErr := email.Active.Send(ctx, emailAddr, subject, body); sendErr != nil {
		// Don't 500 — the row is persisted; the user can hit "resend" once
		// the cooldown elapses. Just log for ops.
		c.JSON(http.StatusOK, gin.H{"ok": true, "warning": "email send failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ConfirmEmailVerify accepts the 6-digit code, stamps email_verified_at, and
// deletes the verification row. Returns 400 on bad/expired code.
func ConfirmEmailVerify(c *gin.Context) {
	var req struct {
		Code string `json:"code" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	code := strings.TrimSpace(req.Code)
	if len(code) != 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid code"})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var storedCode string
	var expiresAt time.Time
	var attempts int
	err := db.DB.QueryRow(ctx,
		`SELECT code, expires_at, attempts FROM email_verifications WHERE user_id = $1`, userID,
	).Scan(&storedCode, &expiresAt, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no code requested"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if attempts >= resetMaxAttempts {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts — request a new code"})
		return
	}
	if time.Now().After(expiresAt) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code expired"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(storedCode)), []byte(code)) != 1 {
		_, _ = db.DB.Exec(ctx,
			`UPDATE email_verifications SET attempts = attempts + 1 WHERE user_id = $1`, userID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "incorrect code"})
		return
	}

	if _, err := db.DB.Exec(ctx,
		`UPDATE users SET email_verified_at = NOW() WHERE id = $1`, userID,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	_, _ = db.DB.Exec(ctx,
		`DELETE FROM email_verifications WHERE user_id = $1`, userID,
	)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
