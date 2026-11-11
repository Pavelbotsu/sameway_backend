package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	config.Load()
	if config.C.JWTSecret == "" {
		config.C.JWTSecret = "admin-test-secret"
	}
	pool, teardown := testutil.MustStartPostgres()
	testPool = pool
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testutil.ResetDB(t, testPool)
	testutil.UseDB(t, testPool)
	return testPool
}

func runCmd(t *testing.T, cmd string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), &out, cmd, args)
	return out.String(), err
}

func TestAdmin_SuspendAndUnsuspend(t *testing.T) {
	pool := setup(t)
	id, _ := testutil.CreateUser(t, pool, "driver", "bad")

	_, err := runCmd(t, "suspend", "bad@test.local")
	assert.Error(t, err, "a reason is required")

	out, err := runCmd(t, "suspend", "bad@test.local", "--reason", "harassment")
	require.NoError(t, err)
	assert.Contains(t, out, "suspended bad@test.local")
	var reason string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT suspended_reason FROM users WHERE id = $1`, id).Scan(&reason))
	assert.Equal(t, "harassment", reason)

	out, err = runCmd(t, "user", id)
	require.NoError(t, err)
	assert.Contains(t, out, "SUSPENDED")

	_, err = runCmd(t, "unsuspend", "bad@test.local")
	require.NoError(t, err)
	out, err = runCmd(t, "user", "bad@test.local")
	require.NoError(t, err)
	assert.Contains(t, out, "status:       active")

	_, err = runCmd(t, "user", "nobody@test.local")
	assert.Error(t, err)
}

func TestAdmin_Lists(t *testing.T) {
	pool := setup(t)
	a, _ := testutil.CreateUser(t, pool, "passenger", "rep-a")
	b, _ := testutil.CreateUser(t, pool, "driver", "rep-b")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO user_reports (reporter_id, reported_id, reason) VALUES ($1, $2, 'rude in chat')`, a, b)
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`INSERT INTO bug_reports (user_id, title, body) VALUES ($1, 'Map blank', 'after login')`, a)
	require.NoError(t, err)

	out, err := runCmd(t, "reports")
	require.NoError(t, err)
	assert.Contains(t, out, "rep-b@test.local")
	assert.Contains(t, out, "rude in chat")

	out, err = runCmd(t, "bugs", "-n", "5")
	require.NoError(t, err)
	assert.Contains(t, out, "Map blank")

	out, err = runCmd(t, "flags")
	require.NoError(t, err)
	assert.Contains(t, out, "FLAGGED BY")

	out, err = runCmd(t, "retention")
	require.NoError(t, err)
	assert.Contains(t, out, "ride_requests")

	_, err = runCmd(t, "nope")
	assert.Error(t, err)
}
