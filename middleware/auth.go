package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// defaultJWTTTLHours is the fallback when JWT_TTL_HOURS is unset or nonsense.
// 168h (7 days) rather than the previous 720h (30 days): a leaked token is
// only as dangerous as it is long-lived, and token_version revocation covers
// the deliberate-logout cases.
const defaultJWTTTLHours = 168

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := tokenFromRequest(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		userID, role, tv, err := ParseToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		st, ok := LoadAccountStatus(c.Request.Context(), userID)
		if !ok || st.TokenVersion != tv {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		// A suspended user may still sign out, export their data and delete
		// their account
		// (GDPR erasure doesn't stop at a suspension).
		if st.Suspended && !suspendedAllowed[c.FullPath()] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "this account is suspended", "code": "account_suspended",
			})
			return
		}
		c.Set("user_id", userID)
		c.Set("role", role)
		c.Set(ctxConsentOK, st.ConsentOK)
		c.Next()
	}
}

const ctxConsentOK = "consent_ok"

// Routes a suspended account can still use: data-subject rights don't stop
// at a suspension.
var suspendedAllowed = map[string]bool{
	"/auth/delete-account": true,
	"/auth/logout":         true,
	"/auth/export":         true,
}

// AccountStatus is what Auth needs to know about the caller on every request,
// read in ONE primary-key lookup (it used to be just token_version).
type AccountStatus struct {
	TokenVersion int
	// ConsentOK: the current Terms/Privacy version is accepted and 18+ was
	// confirmed (see models.CurrentTermsVersion).
	ConsentOK bool
	Suspended bool
}

// LoadAccountStatus reads the caller's status. ok=false means no live row
// (deleted account) or a DB error — callers fail closed.
func LoadAccountStatus(ctx context.Context, userID string) (st AccountStatus, ok bool) {
	st, err := ReadAccountStatus(ctx, userID)
	return st, err == nil
}

// ReadAccountStatus is LoadAccountStatus with the error kept: pgx.ErrNoRows
// means the account is gone (deleted), anything else is a DB problem.
func ReadAccountStatus(ctx context.Context, userID string) (st AccountStatus, err error) {
	// db.DB is nil only in middleware unit tests, which don't stand up a
	// database: treat every token there as version 0, consented, active.
	if db.DB == nil {
		return AccountStatus{ConsentOK: true}, nil
	}
	err = db.DB.QueryRow(ctx, `
		SELECT token_version,
		       terms_version >= $2 AND age_confirmed_at IS NOT NULL,
		       suspended_at IS NOT NULL
		FROM users WHERE id = $1 AND deleted_at IS NULL`,
		userID, models.CurrentTermsVersion,
	).Scan(&st.TokenVersion, &st.ConsentOK, &st.Suspended)
	return st, err
}

// RequireConsent refuses the ride, chat and rating APIs until the caller has
// accepted the current Terms/Privacy and confirmed 18+. Register after Auth.
// The /auth group stays open so the app can still show the gate, record the
// acceptance (/auth/consent), switch role, log out or delete the account.
//
// Without this the consent gate lived only in the app: an old build, or a
// direct API call, used the service without ever accepting.
func RequireConsent() gin.HandlerFunc {
	return func(c *gin.Context) {
		if ok, _ := c.Get(ctxConsentOK); ok != true {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "accept the current terms to continue", "code": "consent_required",
			})
			return
		}
		c.Next()
	}
}

// Session revocation: Auth (and the WS handshake) reject a token whose `tv`
// claim no longer matches users.token_version (see LoadAccountStatus).
//
// JWTs here are stateless, so without this a token stays valid for its full
// lifetime no matter what happens to the account. Bumping token_version is how
// the server revokes outstanding sessions:
//   - password change / reset — otherwise "someone has my account, let me
//     change my password" does not actually evict the attacker;
//   - role switch — otherwise the pre-switch token keeps its old role claim and
//     the user can drive both role APIs at once;
//   - account deletion and suspension.
//
// Tokens minted before this claim existed have tv=0 and remain valid until the
// account's version is first bumped, so the change doesn't force a mass logout.

// BumpTokenVersion invalidates every outstanding token for the user. Runs on
// the caller's queryer so it can be part of an existing transaction.
func BumpTokenVersion(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, userID string) error {
	_, err := q.Exec(ctx,
		`UPDATE users SET token_version = token_version + 1 WHERE id = $1`, userID)
	return err
}

func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("role") != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

// GenerateToken mints a session token stamped with the user's CURRENT
// token_version, so any later bump (password change, reset, role switch)
// invalidates it. The version is read here rather than passed in to keep the
// five call sites unchanged; token minting is rare (login/register/switch/
// reset), so the extra primary-key lookup is not on any hot path.
func GenerateToken(userID, role string) (string, error) {
	ttl := time.Duration(config.C.JWTTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = time.Duration(defaultJWTTTLHours) * time.Hour
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID,
		"role":    role,
		"tv":      currentTokenVersion(context.Background(), userID),
		"iat":     now.Unix(),
		"exp":     now.Add(ttl).Unix(),
	})
	return t.SignedString([]byte(config.C.JWTSecret))
}

// currentTokenVersion reads users.token_version, defaulting to 0 when it can't
// (no pool in unit tests, or the row is gone). A wrong-but-low value here only
// ever mints a token that Auth will then reject.
func currentTokenVersion(ctx context.Context, userID string) int {
	if db.DB == nil {
		return 0
	}
	var v int
	if err := db.DB.QueryRow(ctx,
		`SELECT token_version FROM users WHERE id = $1`, userID,
	).Scan(&v); err != nil {
		return 0
	}
	return v
}

// ParseToken validates the token (signature, and expiry when present — tokens
// issued before the exp claim was introduced remain valid until users
// re-login) and returns the user_id, role and token-version claims. user_id and
// role must be present and strings; a structurally valid token without them is
// rejected rather than panicking downstream.
//
// tokenVersion defaults to 0 when the `tv` claim is absent, which is what
// tokens minted before revocation existed carry. Signature validity is checked
// here; whether the version is still current is LoadAccountStatus' job (checked in Auth).
func ParseToken(tokenStr string) (userID, role string, tokenVersion int, err error) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return []byte(config.C.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
	if err != nil {
		return "", "", 0, err
	}
	if !t.Valid {
		return "", "", 0, errors.New("invalid token")
	}
	claims, ok := t.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", 0, errors.New("unexpected claims type")
	}
	userID, uok := claims["user_id"].(string)
	role, rok := claims["role"].(string)
	if !uok || !rok || userID == "" || role == "" {
		return "", "", 0, errors.New("token missing required claims")
	}
	// JSON numbers decode as float64 through MapClaims.
	if tv, ok := claims["tv"].(float64); ok {
		tokenVersion = int(tv)
	}
	return userID, role, tokenVersion, nil
}

// tokenFromRequest reads the bearer token from the Authorization header ONLY.
//
// The `?token=` query fallback was removed deliberately: query strings land in
// proxy access logs, browser history, and Referer headers, so accepting a JWT
// there leaks it on every request that uses it. The WebSocket handshake still
// reads `?token=` in HandleWS because browsers cannot set headers on a WS
// upgrade — that one path is covered by middleware.Logger not recording the
// query string.
func tokenFromRequest(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}
