package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/routes"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// NewTestRouter returns a Gin engine with the production route table
// (routes.Register) and body limit, minus rate limits: tests hit the same
// endpoints far more often than a user would. NewTestRouterWithRateLimits
// keeps them, for tests of the limits themselves. Uses gin.TestMode (no log
// spam). Hub is the package-level singleton in handlers.
func NewTestRouter() *gin.Engine { return newTestRouter(false) }

// NewTestRouterWithRateLimits is NewTestRouter with production rate limits.
func NewTestRouterWithRateLimits() *gin.Engine { return newTestRouter(true) }

func newTestRouter(rateLimits bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.BodyLimit(64 << 10))
	// Dev routes on: tests run with APP_ENV unset (dev), exactly when
	// main.go registers them too.
	routes.Register(r, routes.Options{RateLimits: rateLimits, DevRoutes: true})
	return r
}

// Do hits the engine with a single HTTP request, encoded as JSON if body is
// non-nil. Authorization header is set when token is non-empty.
func Do(t testing.TB, r *gin.Engine, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// PostJSON is a thin convenience for the most common case.
func PostJSON(t testing.TB, r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodPost, path, token, body)
}

func PutJSON(t testing.TB, r *gin.Engine, path, token string, body any) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodPut, path, token, body)
}

func GetJSON(t testing.TB, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodGet, path, token, nil)
}

func DeleteJSON(t testing.TB, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	return Do(t, r, http.MethodDelete, path, token, nil)
}

// DecodeJSON decodes the recorder body into v.
func DecodeJSON(t testing.TB, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), v), "decode response: %s", w.Body.String())
}
