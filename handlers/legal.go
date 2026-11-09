package handlers

import (
	"context"
	"embed"
	"log"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
)

// CurrentTermsVersion re-exports models.CurrentTermsVersion (the source of
// truth, shared with middleware.RequireConsent).
const CurrentTermsVersion = models.CurrentTermsVersion

// The documents are served by the backend rather than bundled in the app so a
// corrected policy reaches every user without a store release, and so the
// store listings (Play "privacy policy URL", account-deletion URL) have a
// stable public address.
//
//go:embed static/privacy.html static/terms.html static/delete_account.html
var legalFS embed.FS

func serveStatic(name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		b, err := legalFS.ReadFile("static/" + name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")
		c.Data(http.StatusOK, "text/html; charset=utf-8", b)
	}
}

// PrivacyPolicy serves GET /legal/privacy.
var PrivacyPolicy = serveStatic("privacy.html")

// TermsOfService serves GET /legal/terms.
var TermsOfService = serveStatic("terms.html")

// needsConsent reports whether the user still has to accept the current
// Terms/Privacy version or the 18+ attestation. Errors read as "needs
// consent": asking again is harmless, skipping it is not.
func needsConsent(ctx context.Context, userID string) bool {
	var needs bool
	if err := db.DB.QueryRow(ctx, `
		SELECT terms_version < $2 OR age_confirmed_at IS NULL
		FROM users WHERE id = $1`, userID, CurrentTermsVersion,
	).Scan(&needs); err != nil {
		return true
	}
	return needs
}

// AcceptTerms records the signed-in user's acceptance of the current Terms
// and Privacy Policy plus the 18+ attestation. Used by accounts created before
// consent existed and after every CurrentTermsVersion bump.
func AcceptTerms(c *gin.Context) {
	var req struct {
		AcceptTerms  bool `json:"accept_terms"`
		AgeConfirmed bool `json:"age_confirmed"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !req.AcceptTerms || !req.AgeConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "you must be 18 or older and accept the terms to use Sameway",
			"code":  "consent_required",
		})
		return
	}
	userID := c.GetString("user_id")
	if _, err := db.DB.Exec(c.Request.Context(), `
		UPDATE users
		SET terms_version     = $2,
		    terms_accepted_at = NOW(),
		    age_confirmed_at  = COALESCE(age_confirmed_at, NOW())
		WHERE id = $1 AND deleted_at IS NULL`,
		userID, CurrentTermsVersion,
	); err != nil {
		log.Printf("AcceptTerms %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "terms_version": CurrentTermsVersion})
}
