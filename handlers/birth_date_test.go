package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegister_BirthDateRequiredAndAdult(t *testing.T) {
	pool, r := setupHandlers(t)
	body := func(email, dob string) map[string]any {
		b := registerBody("Ann", email, "secret1")
		if dob == "" {
			delete(b, "birth_date")
		} else {
			b["birth_date"] = dob
		}
		return b
	}

	w := testutil.PostJSON(t, r, "/auth/register", "", body("a@test.local", ""))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "birth_date_invalid")

	seventeen := time.Now().AddDate(-18, 0, 1).Format("2006-01-02")
	w = testutil.PostJSON(t, r, "/auth/register", "", body("b@test.local", seventeen))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "underage")

	eighteen := time.Now().AddDate(-18, 0, 0).Format("2006-01-02")
	w = testutil.PostJSON(t, r, "/auth/register", "", body("c@test.local", eighteen))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var stored string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT birth_date::text FROM users WHERE email = 'c@test.local'`).Scan(&stored))
	assert.Equal(t, eighteen, stored)
}

// Accounts from before birth dates were collected go through the consent
// gate once and must give one there.
func TestConsent_LegacyAccountMustGiveBirthDate(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "legacy")
	_, err := pool.Exec(ctx(t), `UPDATE users SET birth_date = NULL, terms_version = 1 WHERE email = 'legacy@test.local'`)
	require.NoError(t, err)

	login := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "legacy@test.local", "password": "test-pass"})
	require.Equal(t, http.StatusOK, login.Code)
	var lr map[string]any
	testutil.DecodeJSON(t, login, &lr)
	assert.Equal(t, true, lr["needs_consent"])
	assert.Equal(t, true, lr["needs_birth_date"])

	// Gated API refused until then.
	w := testutil.PostJSON(t, r, "/trips/search", tok, map[string]any{})
	assert.Equal(t, http.StatusForbidden, w.Code)

	w = testutil.PostJSON(t, r, "/auth/consent", tok, map[string]any{
		"accept_terms": true, "age_confirmed": true})
	assert.Equal(t, http.StatusBadRequest, w.Code, "birth date is required here")

	w = testutil.PostJSON(t, r, "/auth/consent", tok, map[string]any{
		"accept_terms": true, "age_confirmed": true, "birth_date": "1995-05-05"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/trips/search", tok, map[string]any{})
	assert.NotEqual(t, http.StatusForbidden, w.Code)
}

// The other party's age is shared only once a ride is agreed.
func TestAge_OnlyVisibleAfterAccept(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "age-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "age-p")
	reqID, _, err := services.CreateRideRequestWithStatus(driverID, passengerID, 50, 20, "pending")
	require.NoError(t, err)

	reqs, err := services.GetDriverRequests(driverID)
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	assert.Nil(t, reqs[0].PassengerAge, "no age while pending")

	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)
	reqs, err = services.GetDriverRequests(driverID)
	require.NoError(t, err)
	require.NotNil(t, reqs[0].PassengerAge)
	assert.GreaterOrEqual(t, *reqs[0].PassengerAge, 18)
}
