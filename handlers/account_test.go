package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── consent at registration ─────────────────────────────────────────────────

func TestRegister_RequiresConsentAndAge(t *testing.T) {
	_, r := setupHandlers(t)
	base := map[string]any{
		"name": "A", "email": "c@x.com", "password": "secret123", "role": "passenger",
	}
	for name, extra := range map[string]map[string]any{
		"no flags":     {},
		"terms only":   {"accept_terms": true},
		"age only":     {"age_confirmed": true},
		"age declined": {"accept_terms": true, "age_confirmed": false},
	} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		for k, v := range extra {
			body[k] = v
		}
		w := testutil.PostJSON(t, r, "/auth/register", "", body)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
		assert.Contains(t, w.Body.String(), "consent_required", name)
	}
}

func TestRegister_RecordsConsent(t *testing.T) {
	pool, r := setupHandlers(t)
	w := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "A", "email": "rec@x.com", "password": "secret123", "role": "driver",
		"accept_terms": true, "age_confirmed": true,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	assert.Equal(t, false, body["needs_consent"])

	var version int
	var termsAt, ageAt bool
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT terms_version, terms_accepted_at IS NOT NULL, age_confirmed_at IS NOT NULL
		FROM users WHERE email = 'rec@x.com'`).Scan(&version, &termsAt, &ageAt))
	assert.Equal(t, handlers.CurrentTermsVersion, version)
	assert.True(t, termsAt)
	assert.True(t, ageAt)
}

// Accounts created before consent existed report needs_consent on login until
// they accept via /auth/consent.
func TestLogin_LegacyAccountNeedsConsentUntilAccepted(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "legacy")
	makeLegacyAccount(t, id)

	login := func() map[string]any {
		w := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
			"email": "legacy@test.local", "password": "test-pass",
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var b map[string]any
		testutil.DecodeJSON(t, w, &b)
		return b
	}
	assert.Equal(t, true, login()["needs_consent"])

	w := testutil.PostJSON(t, r, "/auth/consent", tok, map[string]any{"accept_terms": true})
	assert.Equal(t, http.StatusBadRequest, w.Code, "age attestation is required too")

	w = testutil.PostJSON(t, r, "/auth/consent", tok, map[string]any{
		"accept_terms": true, "age_confirmed": true,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, false, login()["needs_consent"])
}

// ── in-app deletion ─────────────────────────────────────────────────────────

func TestDeleteAccount_WrongPasswordIs403AndKeepsAccount(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "keep")

	w := testutil.PostJSON(t, r, "/auth/delete-account", tok, map[string]any{"password": "nope"})
	// 403, never 401: the app treats 401 as "session expired" and logs out.
	assert.Equal(t, http.StatusForbidden, w.Code)

	var deleted bool
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&deleted))
	assert.False(t, deleted)
}

func TestDeleteAccount_ActiveRideBlocksDeletion(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "drv")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "pax")
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status, pickup_code)
		VALUES ($1, $2, 'accepted', '1234')`, driverID, paxID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/auth/delete-account", paxTok, map[string]any{"password": "test-pass"})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "active_ride")
}

// seedDeletionFixture gives `pax` one of everything deletion must touch.
func seedDeletionFixture(t *testing.T, pool *pgxpool.Pool) (paxID, paxTok, driverID, offeredID, doneID string) {
	t.Helper()
	driverID, _ = testutil.CreateUser(t, pool, "driver", "drv")
	paxID, paxTok = testutil.CreateUser(t, pool, "passenger", "pax")
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.06, Lng: 19.94}, nil)

	exec := func(q string, args ...any) {
		_, err := pool.Exec(ctx(t), q, args...)
		require.NoError(t, err, q)
	}
	exec(`UPDATE users SET phone = '+48 600 000 000', bio = 'hi', fcm_token = 'tok',
	      car_plate = 'KR 1', photo_url = 'https://x/y.jpg' WHERE id = $1`, paxID)
	require.NoError(t, pool.QueryRow(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status, pickup_location, pickup_code)
		VALUES ($1, $2, 'done', ST_MakePoint(19.94, 50.06)::geography, '4321') RETURNING id`,
		driverID, paxID).Scan(&doneID))
	require.NoError(t, pool.QueryRow(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status, pickup_location)
		VALUES ($1, $2, 'offered', ST_MakePoint(19.95, 50.07)::geography) RETURNING id`,
		driverID, paxID).Scan(&offeredID))
	exec(`INSERT INTO ratings (rater_id, rated_id, ride_request_id, stars, comment)
	      VALUES ($1, $2, $3, 4, 'great driver'), ($2, $1, $3, 5, 'great passenger')`,
		paxID, driverID, doneID)
	var convID string
	require.NoError(t, pool.QueryRow(ctx(t), `
		INSERT INTO conversations (user_a, user_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid)) RETURNING id`,
		paxID, driverID).Scan(&convID))
	exec(`INSERT INTO messages (conversation_id, sender_id, content) VALUES ($1, $2, 'hello'), ($1, $3, 'hey')`,
		convID, paxID, driverID)
	exec(`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)`, driverID, paxID)
	exec(`INSERT INTO bug_reports (user_id, title, body) VALUES ($1, 't', 'b')`, paxID)
	exec(`INSERT INTO user_reports (reporter_id, reported_id, reason) VALUES ($1, $2, 'rude')`, driverID, paxID)
	return
}

func TestDeleteAccount_ScrubsPersonalDataAndRevokesAccess(t *testing.T) {
	pool, r := setupHandlers(t)
	paxID, paxTok, driverID, offeredID, doneID := seedDeletionFixture(t, pool)

	w := testutil.PostJSON(t, r, "/auth/delete-account", paxTok, map[string]any{"password": "test-pass"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Identity scrubbed, row kept for the counterparty's history.
	var name, email, phone, bio, plate string
	var fcm, photo *string
	var deleted bool
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT name, email, phone, bio, car_plate, fcm_token, photo_url, deleted_at IS NOT NULL
		FROM users WHERE id = $1`, paxID).Scan(&name, &email, &phone, &bio, &plate, &fcm, &photo, &deleted))
	assert.Equal(t, "Deleted user", name)
	assert.True(t, strings.HasSuffix(email, "@deleted.invalid"))
	assert.Empty(t, phone)
	assert.Empty(t, bio)
	assert.Empty(t, plate)
	assert.Nil(t, fcm)
	assert.Nil(t, photo)
	assert.True(t, deleted)

	count := func(q string, args ...any) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx(t), q, args...).Scan(&n), q)
		return n
	}
	assert.Zero(t, count(`SELECT COUNT(*) FROM active_passengers WHERE passenger_id = $1`, paxID))
	assert.Zero(t, count(`SELECT COUNT(*) FROM conversations WHERE user_a = $1 OR user_b = $1`, paxID))
	assert.Zero(t, count(`SELECT COUNT(*) FROM messages WHERE sender_id = $1`, paxID))
	assert.Zero(t, count(`SELECT COUNT(*) FROM user_blocks WHERE blocker_id = $1 OR blocked_id = $1`, paxID))
	assert.Zero(t, count(`SELECT COUNT(*) FROM bug_reports WHERE user_id = $1`, paxID))
	// Location history and the code are gone from past rides; the ride stays.
	assert.Zero(t, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1 AND (pickup_location IS NOT NULL OR pickup_code IS NOT NULL)`, doneID))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1`, doneID))
	// Open offers are cancelled.
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1 AND status = 'cancelled'`, offeredID))
	// Their rating comment is gone; stars and the rating they RECEIVED stay.
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM ratings WHERE rater_id = $1 AND comment IS NULL AND stars = 4`, paxID))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM ratings WHERE rater_id = $1 AND comment = 'great passenger'`, driverID))
	// Safety record about them survives deletion.
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM user_reports WHERE reported_id = $1`, paxID))

	// The old session is dead, and so is the login.
	assert.Equal(t, http.StatusUnauthorized, testutil.GetJSON(t, r, "/auth/profile", paxTok).Code)
	login := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "pax@test.local", "password": "test-pass",
	})
	assert.Equal(t, http.StatusUnauthorized, login.Code)

	// The address is free to register again.
	re := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "New", "email": "pax@test.local", "password": "secret123", "role": "passenger",
		"accept_terms": true, "age_confirmed": true,
	})
	assert.Equal(t, http.StatusCreated, re.Code, re.Body.String())
}

func TestDeleteAccount_NotifiesDriverOfCancelledPendingRequest(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "drv")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "pax")
	var reqID string
	require.NoError(t, pool.QueryRow(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'pending') RETURNING id`, driverID, paxID).Scan(&reqID))

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, driverTok)
	defer conn.Close()

	w := testutil.PostJSON(t, r, "/auth/delete-account", paxTok, map[string]any{"password": "test-pass"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	payload := testutil.ReadEventOfType(t, conn, 2*time.Second, "passenger_request_cancelled")
	assert.Equal(t, reqID, payload["request_id"])
	assert.Equal(t, "passenger_account_deleted", payload["cancelled_by"])
}

// ── web deletion page (Play "delete account" URL) ───────────────────────────

func postForm(r *gin.Engine, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWebDeletion(t *testing.T) {
	pool, r := setupHandlers(t)
	id, _ := testutil.CreateUser(t, pool, "passenger", "webuser")

	page := testutil.GetJSON(t, r, "/account/delete", "")
	assert.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), `action="/account/delete"`)

	// Unknown email and wrong password look identical (no account probing).
	unknown := postForm(r, "/account/delete", url.Values{"email": {"nobody@x.com"}, "password": {"x"}})
	wrong := postForm(r, "/account/delete", url.Values{"email": {"webuser@test.local"}, "password": {"x"}})
	assert.Equal(t, http.StatusUnauthorized, unknown.Code)
	assert.Equal(t, unknown.Body.String(), wrong.Body.String())

	ok := postForm(r, "/account/delete", url.Values{"email": {"WebUser@test.local "}, "password": {"test-pass"}})
	assert.Equal(t, http.StatusOK, ok.Code)
	assert.Contains(t, ok.Body.String(), "Account deleted")

	var deleted bool
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, id).Scan(&deleted))
	assert.True(t, deleted)
}

func TestLegalPagesAreServed(t *testing.T) {
	_, r := setupHandlers(t)
	for _, p := range []string{"/legal/privacy", "/legal/terms"} {
		w := testutil.GetJSON(t, r, p, "")
		assert.Equal(t, http.StatusOK, w.Code, p)
		assert.Contains(t, w.Header().Get("Content-Type"), "text/html", p)
	}
}
