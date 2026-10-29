// Package testutil provides shared scaffolding for the backend test suite:
// container lifecycle, seeders, mock OSRM, HTTP and WS helpers. None of this
// is imported by production code.
package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// loadTestConfig populates config.C with env-or-default values so matching
// tunables (Match*) aren't the zero value during tests. Production calls
// config.Load() from main; tests go through the container harness, so we load
// here. No .env in the test working dir → pure defaults, which is what the
// matching tests assert against.
func loadTestConfig() { config.Load() }

// StartPostgres boots a fresh Postgres+PostGIS container, runs the production
// migration against it, and returns a pool. The container is torn down via
// t.Cleanup. Intended to be called from a package's TestMain (one container
// per package, shared across tests, isolated via ResetDB).
func StartPostgres(t testing.TB) *pgxpool.Pool {
	t.Helper()
	loadTestConfig()
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgis/postgis:15-3.4-alpine",
		postgres.WithDatabase("sameway_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgis container")
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "open pool")
	t.Cleanup(pool.Close)

	require.NoError(t, db.MigratePool(ctx, pool), "run migration")
	return pool
}

// ResetDB truncates every table used by the app, restarting identity columns.
// Call from each test's setup so state from one test doesn't leak into the
// next. PostGIS extension and migration metadata are preserved.
func ResetDB(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	// password_resets is keyed by EMAIL with no FK to users, so truncating
	// users does not cascade to it — reset codes and their attempt counters
	// leaked between tests. email_verifications does cascade, but is listed
	// explicitly so the set is obvious rather than incidental.
	_, err := pool.Exec(context.Background(), `
		TRUNCATE TABLE
			ratings, messages, completed_trips, ride_requests,
			planned_trips, active_passengers, active_drivers, users,
			conversations, user_blocks, user_reports,
			transit_stops, match_events,
			password_resets, email_verifications
		RESTART IDENTITY CASCADE
	`)
	require.NoError(t, err, "reset db")
}

// UseDB swaps the global db.DB pool to the test pool for the duration of the
// test (restores on Cleanup). Required because production code uses the global
// pool. Concurrency caveat: tests sharing the global must run sequentially
// within a package — tests rely on t.Parallel only when they don't touch DB.
func UseDB(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	prev := db.DB
	db.DB = pool
	t.Cleanup(func() { db.DB = prev })
}

// MustStartPostgres is the TestMain-friendly variant of StartPostgres. It
// doesn't take a *testing.TB (which can't be implemented externally) — instead
// it returns the pool plus a teardown function that callers must defer. Panics
// on any error since TestMain has no way to gracefully fail.
func MustStartPostgres() (*pgxpool.Pool, func()) {
	loadTestConfig()
	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgis/postgis:15-3.4-alpine",
		postgres.WithDatabase("sameway_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		panic("testutil.MustStartPostgres: " + err.Error())
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic("testutil.MustStartPostgres: dsn: " + err.Error())
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = container.Terminate(ctx)
		panic("testutil.MustStartPostgres: pool: " + err.Error())
	}
	if err := db.MigratePool(ctx, pool); err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic("testutil.MustStartPostgres: migrate: " + err.Error())
	}
	teardown := func() {
		pool.Close()
		_ = container.Terminate(ctx)
	}
	return pool, teardown
}
