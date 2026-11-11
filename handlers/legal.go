package handlers

import (
	"context"
	"embed"
	"log"
	"net/http"
	"strings"
	"time"

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
//go:embed static/*.html static/style.css
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

// PrivacyPolicy serves GET /legal/privacy[?lang=en|pl|uk].
var PrivacyPolicy = legalDoc("privacy")

// TermsOfService serves GET /legal/terms[?lang=en|pl|uk].
var TermsOfService = legalDoc("terms")

// LegalStyle serves GET /legal/style.css, shared by the documents.
func LegalStyle(c *gin.Context) {
	b, err := legalFS.ReadFile("static/style.css")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "text/css; charset=utf-8", b)
}

// legalLangs are the languages the documents exist in.
var legalLangs = map[string]bool{"en": true, "pl": true, "uk": true}

// legalLang picks the document language: ?lang= (the app passes its UI
// language), else the browser's Accept-Language, else English. Russian
// readers get Ukrainian: the service runs in Poland and Ukraine only.
func legalLang(c *gin.Context) string {
	if l := strings.ToLower(c.Query("lang")); legalLangs[l] {
		return l
	}
	for _, part := range strings.Split(c.GetHeader("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if len(tag) < 2 {
			continue
		}
		l := tag[:2]
		if l == "ru" {
			l = "uk"
		}
		if legalLangs[l] {
			return l
		}
	}
	return "en"
}

// legalDoc serves static/<doc>.<lang>.html in the reader's language.
func legalDoc(doc string) gin.HandlerFunc {
	return func(c *gin.Context) {
		b, err := legalFS.ReadFile("static/" + doc + "." + legalLang(c) + ".html")
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Cache-Control", "public, max-age=3600")
		c.Header("Vary", "Accept-Language")
		c.Data(http.StatusOK, "text/html; charset=utf-8", b)
	}
}

// needsConsent reports whether the user still has to accept the current
// Terms/Privacy version or the 18+ attestation. Errors read as "needs
// consent": asking again is harmless, skipping it is not.
func needsConsent(ctx context.Context, userID string) bool {
	var needs bool
	if err := db.DB.QueryRow(ctx, `
		SELECT terms_version < $2 OR age_confirmed_at IS NULL OR birth_date IS NULL
		FROM users WHERE id = $1`, userID, CurrentTermsVersion,
	).Scan(&needs); err != nil {
		return true
	}
	return needs
}

// needsBirthDate reports whether the consent screen must also ask for the
// date of birth (accounts created before it was collected).
func needsBirthDate(ctx context.Context, userID string) bool {
	var needs bool
	if err := db.DB.QueryRow(ctx,
		`SELECT birth_date IS NULL FROM users WHERE id = $1`, userID,
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
		// Required when the account has no date of birth yet; ignored
		// otherwise (it can't be changed here once set).
		BirthDate string `json:"birth_date"`
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
	ctx := c.Request.Context()
	var birthDate *time.Time
	if needsBirthDate(ctx, userID) {
		d, code, msg := parseBirthDate(req.BirthDate, time.Now())
		if code != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg, "code": code})
			return
		}
		birthDate = &d
	}
	if _, err := db.DB.Exec(ctx, `
		UPDATE users
		SET terms_version     = $2,
		    terms_accepted_at = NOW(),
		    age_confirmed_at  = COALESCE(age_confirmed_at, NOW()),
		    birth_date        = COALESCE(birth_date, $3)
		WHERE id = $1 AND deleted_at IS NULL`,
		userID, CurrentTermsVersion, birthDate,
	); err != nil {
		log.Printf("AcceptTerms %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "terms_version": CurrentTermsVersion})
}
