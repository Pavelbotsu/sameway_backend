package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
)

// The production route table with its rate limits (routes.Register). Other
// tests run without limits, so this is what proves the wiring.
func TestRateLimits_LoginPerIP(t *testing.T) {
	_, _ = setupHandlers(t)
	r := testutil.NewTestRouterWithRateLimits()
	body := map[string]any{"email": "nobody@test.local", "password": "wrong-pass"}
	for i := 0; i < 10; i++ {
		w := testutil.PostJSON(t, r, "/auth/login", "", body)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, testutil.PostJSON(t, r, "/auth/login", "", body).Code)
}

func TestRateLimits_RatingsPerUser(t *testing.T) {
	pool, _ := setupHandlers(t)
	r := testutil.NewTestRouterWithRateLimits()
	_, tok := testutil.CreateUser(t, pool, "passenger", "rl-1")
	body := map[string]any{"rated_id": "00000000-0000-0000-0000-000000000000",
		"ride_request_id": "00000000-0000-0000-0000-000000000000", "stars": 5}
	for i := 0; i < 10; i++ {
		w := testutil.PostJSON(t, r, "/ratings", tok, body)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code, "attempt %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, testutil.PostJSON(t, r, "/ratings", tok, body).Code)

	// Another user has their own budget.
	_, other := testutil.CreateUser(t, pool, "passenger", "rl-2")
	assert.NotEqual(t, http.StatusTooManyRequests, testutil.PostJSON(t, r, "/ratings", other, body).Code)
}
