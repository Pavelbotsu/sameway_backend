package handlers_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeLegacyAccount turns a test user into one created before consent
// existed: no accepted terms version, no 18+ attestation.
func makeLegacyAccount(t *testing.T, userID string) {
	t.Helper()
	_, err := handlersPool.Exec(ctx(t), `
		UPDATE users SET terms_version = 0, terms_accepted_at = NULL, age_confirmed_at = NULL
		WHERE id = $1`, userID)
	require.NoError(t, err)
}

func suspend(t *testing.T, userID string) {
	t.Helper()
	_, err := handlersPool.Exec(ctx(t),
		`UPDATE users SET suspended_at = NOW(), suspended_reason = 'test' WHERE id = $1`, userID)
	require.NoError(t, err)
}

// ── consent enforced server-side (not only by the app's gate) ───────────────

func TestRequireConsent_BlocksRideAPIsUntilAccepted(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "legacy")
	makeLegacyAccount(t, id)
	loc := map[string]any{"lat": 50.06, "lng": 19.94}

	w := testutil.PutJSON(t, r, "/passenger/location", tok, loc)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "consent_required")
	for _, path := range []string{"/chat/conversations"} {
		w := testutil.GetJSON(t, r, path, tok)
		assert.Equal(t, http.StatusForbidden, w.Code, path)
	}

	// The account area stays usable so the app can show and record consent.
	assert.Equal(t, http.StatusOK, testutil.GetJSON(t, r, "/auth/profile", tok).Code)

	w = testutil.PostJSON(t, r, "/auth/consent", tok, map[string]any{
		"accept_terms": true, "age_confirmed": true, "birth_date": "1990-01-01",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Same token keeps working: consent doesn't revoke the session.
	w = testutil.PutJSON(t, r, "/passenger/location", tok, loc)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// A safety report must never be blocked by a terms prompt.
func TestRequireConsent_NeverBlocksSafetyFlag(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "legacy")
	makeLegacyAccount(t, id)
	w := testutil.PostJSON(t, r, "/safety/flag", tok, map[string]any{
		"request_id": "00000000-0000-0000-0000-000000000000",
	})
	assert.NotContains(t, w.Body.String(), "consent_required")
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── suspension ───────────────────────────────────────────────────────────────

func TestSuspendedAccount_LockedOutExceptDeletion(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "bad")
	suspend(t, id)

	w := testutil.GetJSON(t, r, "/auth/profile", tok)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "account_suspended")

	login := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "bad@test.local", "password": "test-pass",
	})
	assert.Equal(t, http.StatusForbidden, login.Code)
	assert.Contains(t, login.Body.String(), "account_suspended")
	// Wrong password still looks like any other failed login.
	wrong := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "bad@test.local", "password": "nope",
	})
	assert.Equal(t, http.StatusUnauthorized, wrong.Code)

	// No live event stream either.
	wsBase := testutil.ServeWS(t, r)
	_, resp, err := websocket.DefaultDialer.Dial(wsBase+"/ws?token="+tok, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// Erasure still works.
	del := testutil.PostJSON(t, r, "/auth/delete-account", tok, map[string]any{"password": "test-pass"})
	assert.Equal(t, http.StatusOK, del.Code, del.Body.String())
}

// ── profile fields the account screen saves ─────────────────────────────────

const firebasePhoto = "https://firebasestorage.googleapis.com/v0/b/sameway.appspot.com/o/user_photos%2Fx.jpg?alt=media&token=abc"

func TestUpdateProfile_SavesPhotoBioPhone(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")

	w := testutil.PutJSON(t, r, "/auth/profile", tok, map[string]any{
		"photo_url": firebasePhoto, "bio": "  Commuting to AGH  ", "phone": "+48 600 100 200",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var prof map[string]any
	testutil.DecodeJSON(t, testutil.GetJSON(t, r, "/auth/profile", tok), &prof)
	assert.Equal(t, firebasePhoto, prof["photo_url"])
	assert.Equal(t, "Commuting to AGH", prof["bio"])
	assert.Equal(t, "+48 600 100 200", prof["phone"])

	// "" removes the photo; omitted fields are untouched.
	w = testutil.PutJSON(t, r, "/auth/profile", tok, map[string]any{"photo_url": ""})
	require.Equal(t, http.StatusOK, w.Code)
	testutil.DecodeJSON(t, testutil.GetJSON(t, r, "/auth/profile", tok), &prof)
	assert.Equal(t, "", prof["photo_url"])
	assert.Equal(t, "Commuting to AGH", prof["bio"])
}

func TestUpdateProfile_RejectsBadValues(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	for name, body := range map[string]map[string]any{
		"foreign photo host": {"photo_url": "https://evil.example.com/x.jpg"},
		"plain http photo":   {"photo_url": "http://firebasestorage.googleapis.com/x.jpg"},
		"bio too long":       {"bio": strings.Repeat("x", 161)},
		"phone letters":      {"phone": "call me maybe"},
		"name too long":      {"name": strings.Repeat("n", 81)},
		"plate too long":     {"car_plate": strings.Repeat("K", 41)},
	} {
		w := testutil.PutJSON(t, r, "/auth/profile", tok, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, name)
	}
}
