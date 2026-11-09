package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogout_RevokesTokenAndPushToken(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	_, err := pool.Exec(ctx(t), `UPDATE users SET fcm_token = 'device-token' WHERE id = $1`, id)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/auth/logout", tok, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// The token no longer works…
	assert.Equal(t, http.StatusUnauthorized, testutil.GetJSON(t, r, "/auth/profile", tok).Code)
	// …and the device no longer gets this account's pushes.
	var fcm *string
	require.NoError(t, pool.QueryRow(ctx(t), `SELECT fcm_token FROM users WHERE id = $1`, id).Scan(&fcm))
	assert.Nil(t, fcm)

	// Logging in again works normally.
	login := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "p1@test.local", "password": "test-pass",
	})
	assert.Equal(t, http.StatusOK, login.Code)
}

func TestLogout_AllowedForSuspendedAccount(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	suspend(t, id)
	assert.Equal(t, http.StatusOK, testutil.PostJSON(t, r, "/auth/logout", tok, nil).Code)
}
