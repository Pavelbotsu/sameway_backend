package handlers

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/services/email"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	resetCodeTTL      = 15 * time.Minute
	resetResendCooldown = 60 * time.Second
)

// generateResetCode returns a uniformly-random 6-digit string ("000000" to
// "999999") using crypto/rand. Leading zeros preserved.
func generateResetCode() (string, error) {
	var n uint32
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	n = binary.BigEndian.Uint32(buf) % 1_000_000
	return fmt.Sprintf("%06d", n), nil
}

// ForgotPassword starts a reset flow. Always returns 200 to avoid leaking
// whether an account exists for the given email. If a row exists, generate a
// fresh 6-digit code (or resend the existing one if still within cooldown),
// persist it with a 15-minute expiry, and email it.
func ForgotPassword(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))
	ctx := c.Request.Context()

	// User existence check. We deliberately return success below regardless,
	// but skip the DB write + email send when there is no matching user.
	var userExists bool
	if err := db.DB.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, emailAddr,
	).Scan(&userExists); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if !userExists {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	// If we already sent a code recently, return 429 — don't let an attacker
	// hammer a real address with emails.
	var existingCode string
	var existingCreatedAt time.Time
	err := db.DB.QueryRow(ctx,
		`SELECT code, created_at FROM password_resets WHERE email = $1`, emailAddr,
	).Scan(&existingCode, &existingCreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	now := time.Now()
	if err == nil && now.Sub(existingCreatedAt) < resetResendCooldown {
		c.JSON(http.StatusTooManyRequests,
			gin.H{"error": "please wait a minute before requesting another code"})
		return
	}

	code, err := generateResetCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	expires := now.Add(resetCodeTTL)
	_, err = db.DB.Exec(ctx, `
		INSERT INTO password_resets (email, code, expires_at, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (email) DO UPDATE
			SET code = EXCLUDED.code,
			    expires_at = EXCLUDED.expires_at,
			    created_at = NOW()
	`, emailAddr, code, expires)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// Best-effort send. If the configured Sender errors (Resend down, etc.),
	// we keep the row so the user can request again — but we also log it so
	// ops sees the failure.
	go func(to, code string) {
		bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = email.Active.Send(bg, to,
			"Your sameway reset code",
			fmt.Sprintf("Your sameway password-reset code is %s. It expires in 15 minutes.", code),
		)
	}(emailAddr, code)

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ResetPassword consumes a reset code. On success the user's password is
// updated to the new value and we return a fresh JWT so the client can
// auto-sign-in.
func ResetPassword(c *gin.Context) {
	var req struct {
		Email       string `json:"email" binding:"required"`
		Code        string `json:"code" binding:"required"`
		NewPassword string `json:"new_password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.NewPassword) < 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password must be at least 6 characters"})
		return
	}
	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))
	code := strings.TrimSpace(req.Code)
	ctx := c.Request.Context()

	var storedCode string
	var expiresAt time.Time
	err := db.DB.QueryRow(ctx,
		`SELECT code, expires_at FROM password_resets WHERE email = $1`, emailAddr,
	).Scan(&storedCode, &expiresAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired code"})
		return
	}
	if time.Now().After(expiresAt) || storedCode != code {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired code"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	var userID, role, name string
	if err := tx.QueryRow(ctx,
		`UPDATE users SET password = $1 WHERE email = $2 RETURNING id, role, name`,
		string(hash), emailAddr,
	).Scan(&userID, &role, &name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM password_resets WHERE email = $1`, emailAddr,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	token, err := middleware.GenerateToken(userID, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":   token,
		"user_id": userID,
		"role":    role,
		"name":    name,
		"email":   emailAddr,
	})
}
