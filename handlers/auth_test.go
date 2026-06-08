package handlers_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handlersPool is shared across the whole handlers package's tests.
var handlersPool *pgxpool.Pool

func TestMain(m *testing.M) {
	config.Load()
	if config.C.JWTSecret == "" {
		config.C.JWTSecret = "handlers-test-secret"
	}
	pool, teardown := testutil.MustStartPostgres()
	handlersPool = pool
	code := m.Run()
	teardown()
	os.Exit(code)
}

// setupHandlers truncates state and returns the test pool + a fresh router.
func setupHandlers(t *testing.T) (*pgxpool.Pool, *gin.Engine) {
	t.Helper()
	testutil.ResetDB(t, handlersPool)
	testutil.UseDB(t, handlersPool)
	return handlersPool, testutil.NewTestRouter()
}

// ── /auth/register ──────────────────────────────────────────────────────────

func TestRegister_Valid(t *testing.T) {
	_, r := setupHandlers(t)
	w := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "Alice", "email": "alice@x.com",
		"password": "secret123", "role": "passenger",
	})
	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	assert.NotEmpty(t, body["token"])
	assert.Equal(t, "passenger", body["role"])
}

func TestRegister_DuplicateEmail(t *testing.T) {
	_, r := setupHandlers(t)
	body := map[string]any{
		"name": "A", "email": "dup@x.com",
		"password": "secret123", "role": "passenger",
	}
	w1 := testutil.PostJSON(t, r, "/auth/register", "", body)
	require.Equal(t, http.StatusCreated, w1.Code)
	w2 := testutil.PostJSON(t, r, "/auth/register", "", body)
	assert.Equal(t, http.StatusConflict, w2.Code)
}

func TestRegister_BadPassword(t *testing.T) {
	_, r := setupHandlers(t)
	w := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "A", "email": "a@x.com",
		"password": "short", "role": "passenger",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRegister_BadEmail(t *testing.T) {
	_, r := setupHandlers(t)
	w := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "A", "email": "not-an-email",
		"password": "secret123", "role": "driver",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── /auth/login ─────────────────────────────────────────────────────────────

func TestLogin_GoodPassword(t *testing.T) {
	_, r := setupHandlers(t)
	register := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "A", "email": "login@x.com",
		"password": "secret123", "role": "driver",
	})
	require.Equal(t, http.StatusCreated, register.Code)

	w := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "login@x.com", "password": "secret123",
	})
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	assert.NotEmpty(t, body["token"])
}

func TestLogin_BadPassword(t *testing.T) {
	_, r := setupHandlers(t)
	testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": "A", "email": "login2@x.com",
		"password": "secret123", "role": "driver",
	})
	w := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "login2@x.com", "password": "WRONG",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── /auth/profile ───────────────────────────────────────────────────────────

func TestUpdateProfile_RenameSucceeds(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "old-name")

	w := testutil.PutJSON(t, r, "/auth/profile", tok, map[string]any{"name": "new-name"})
	assert.Equal(t, http.StatusOK, w.Code)

	var name string
	require.NoError(t, pool.QueryRow(ctx(t), `SELECT name FROM users WHERE id = $1`, id).Scan(&name))
	assert.Equal(t, "new-name", name)
}

// ── /auth/switch-role ───────────────────────────────────────────────────────

func TestSwitchRole_NoLiveState(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "u1")

	w := testutil.PostJSON(t, r, "/auth/switch-role", tok, map[string]any{"role": "passenger"})
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	assert.Equal(t, "passenger", body["role"])
	assert.Equal(t, id, body["user_id"])
}

func TestSwitchRole_CleansUpActiveDriver(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "u1")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)

	// Sanity: row exists.
	var n int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM active_drivers WHERE driver_id = $1`, id).Scan(&n))
	require.Equal(t, 1, n)

	w := testutil.PostJSON(t, r, "/auth/switch-role", tok, map[string]any{"role": "passenger"})
	assert.Equal(t, http.StatusOK, w.Code)

	// After switching, the active_drivers row must be gone.
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM active_drivers WHERE driver_id = $1`, id).Scan(&n))
	assert.Equal(t, 0, n, "old role's state must be cleaned up")
}

// ── /auth/change-password ───────────────────────────────────────────────────

func TestChangePassword_BadCurrent(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "u1")
	w := testutil.PostJSON(t, r, "/auth/change-password", tok, map[string]any{
		"current_password": "WRONG", "new_password": "new-pass-1",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChangePassword_GoodRoundTrip(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "u1")

	w := testutil.PostJSON(t, r, "/auth/change-password", tok, map[string]any{
		"current_password": "test-pass", "new_password": "new-pass-1",
	})
	assert.Equal(t, http.StatusOK, w.Code)

	// Log in with the new password.
	w2 := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
		"email": "u1@test.local", "password": "new-pass-1",
	})
	assert.Equal(t, http.StatusOK, w2.Code)
}

// ── /auth/fcm-status (no FCM in test env) ──────────────────────────────────

func TestFCMStatus_DisabledInTests(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "u1")
	w := testutil.GetJSON(t, r, "/auth/fcm-status", tok)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"fcm_enabled":false`)
}
