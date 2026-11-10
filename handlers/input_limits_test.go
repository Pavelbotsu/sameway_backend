package handlers_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func registerBody(name, email, password string) map[string]any {
	return map[string]any{
		"name": name, "email": email, "password": password, "role": "passenger",
		"accept_terms": true, "age_confirmed": true, "birth_date": "1990-01-01",
	}
}

func TestRegister_RejectsBadNameAndOverlongPassword(t *testing.T) {
	_, r := setupHandlers(t)

	w := testutil.PostJSON(t, r, "/auth/register", "", registerBody("   ", "a@test.local", "secret1"))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/auth/register", "", registerBody(strings.Repeat("x", 81), "b@test.local", "secret1"))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Over bcrypt's 72-byte limit: a clear 400, not a 500.
	w = testutil.PostJSON(t, r, "/auth/register", "", registerBody("Ann", "c@test.local", strings.Repeat("p", 73)))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/auth/register", "", registerBody("  Ann  ", "d@test.local", "secret1"))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

func TestChangePassword_RejectsOverlongPassword(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	w := testutil.PostJSON(t, r, "/auth/change-password", tok, map[string]any{
		"current_password": "test-pass", "new_password": strings.Repeat("p", 73),
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// An unknown email must cost about as much as a wrong password, otherwise
// response time tells an attacker which addresses have accounts.
func TestLogin_UnknownEmailTakesBcryptTime(t *testing.T) {
	pool, r := setupHandlers(t)
	testutil.CreateUser(t, pool, "passenger", "p1")

	timed := func(email string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			w := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{"email": email, "password": "wrong-pass"})
			require.Equal(t, http.StatusUnauthorized, w.Code)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	known := timed("p1@test.local")
	unknown := timed("nobody@test.local")
	assert.Greater(t, unknown, known/3,
		"unknown email answered in %v, wrong password in %v", unknown, known)
}

func TestSubmitRating_RejectsOverlongComment(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'done' WHERE id = $1`, reqID)
	require.NoError(t, err)

	body := map[string]any{
		"rated_id": driverID, "ride_request_id": reqID, "stars": 4,
		"comment": strings.Repeat("ą", 501),
	}
	assert.Equal(t, http.StatusBadRequest, testutil.PostJSON(t, r, "/ratings", tok, body).Code)

	body["comment"] = strings.Repeat("ą", 500)
	assert.Equal(t, http.StatusCreated, testutil.PostJSON(t, r, "/ratings", tok, body).Code)
}
