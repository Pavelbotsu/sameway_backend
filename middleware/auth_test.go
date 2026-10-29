package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupConfig() {
	gin.SetMode(gin.TestMode)
	config.C.JWTSecret = "test-secret-for-middleware"
}

// newAuthedRouter is a tiny harness: GET /me responds with the user_id+role
// that middleware.Auth wrote into the gin context.
func newAuthedRouter(extra ...gin.HandlerFunc) *gin.Engine {
	setupConfig()
	r := gin.New()
	group := r.Group("/", append([]gin.HandlerFunc{middleware.Auth()}, extra...)...)
	group.GET("/me", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"user_id": c.GetString("user_id"),
			"role":    c.GetString("role"),
		})
	})
	return r
}

func TestAuth_ValidToken(t *testing.T) {
	r := newAuthedRouter()
	tok, err := middleware.GenerateToken("user-1", "driver")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"user_id":"user-1"`)
	assert.Contains(t, w.Body.String(), `"role":"driver"`)
}

func TestAuth_MissingHeader(t *testing.T) {
	r := newAuthedRouter()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_BearerPrefixMissing(t *testing.T) {
	r := newAuthedRouter()
	tok, _ := middleware.GenerateToken("user-1", "driver")
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", tok) // missing "Bearer "
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// A token in the query string must NOT authenticate. Query strings land in
// proxy access logs, browser history, and Referer headers, so honouring one
// here leaked a 30-day session token on every request that used it. The
// WebSocket handshake is the sole exception and reads `?token=` itself in
// HandleWS, because browsers cannot set headers on a WS upgrade.
func TestAuth_QueryTokenRejected(t *testing.T) {
	r := newAuthedRouter()
	tok, _ := middleware.GenerateToken("user-1", "driver")
	req := httptest.NewRequest(http.MethodGet, "/me?token="+tok, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_WrongSecret(t *testing.T) {
	r := newAuthedRouter()
	// Mint with a different secret.
	tampered := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": "user-1", "role": "driver",
	})
	bad, err := tampered.SignedString([]byte("other-secret"))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+bad)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole_WrongRoleForbidden(t *testing.T) {
	r := newAuthedRouter(middleware.RequireRole("driver"))
	tok, _ := middleware.GenerateToken("user-1", "passenger")
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireRole_CorrectRolePassesThrough(t *testing.T) {
	r := newAuthedRouter(middleware.RequireRole("driver"))
	tok, _ := middleware.GenerateToken("user-1", "driver")
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
