package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
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
		if !TokenVersionValid(c.Request.Context(), userID, tv) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
			return
		}
		c.Set("user_id", userID)
		c.Set("role", role)
		c.Next()
	}
}

// TokenVersionValid reports whether the token's `tv` claim still matches the
// user's current users.token_version.
//
// JWTs here are stateless, so without this a token stays valid for its full
// lifetime no matter what happens to the account. Bumping token_version is how
// the server revokes outstanding sessions:
//   - password change / reset — otherwise "someone has my account, let me
//     change my password" does not actually evict the attacker;
//   - role switch — otherwise the pre-switch token keeps its old role claim and
//     the user can drive both role APIs at once.
//
// Tokens minted before this claim existed have tv=0 and remain valid until the
// account's version is first bumped, so the change doesn't force a mass logout.
func TokenVersionValid(ctx context.Context, userID string, tv int) bool {
	// db.DB is nil only in middleware unit tests, which don't stand up a
	// database. main() calls log.Fatalf when the pool can't be opened, so a
	// running server always has one.
	if db.DB == nil {
		return true
	}
	var current int
	if err := db.DB.QueryRow(ctx,
		`SELECT token_version FROM users WHERE id = $1`, userID,
	).Scan(&current); err != nil {
		// No row (deleted account) or a DB error: fail closed.
		return false
	}
	return current == tv
}

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
// ever mints a token that TokenVersionValid will then reject.
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
// here; whether the version is still current is TokenVersionValid's job.
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
