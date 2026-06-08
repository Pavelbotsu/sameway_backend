package db_test

import (
	"context"
	"os"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var dbPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, teardown := testutil.MustStartPostgres()
	dbPool = pool
	code := m.Run()
	teardown()
	os.Exit(code)
}

func TestMigratePool_IdempotentRerun(t *testing.T) {
	// Re-run migration on the existing container — must succeed (every CREATE
	// uses IF NOT EXISTS, the partial index uses IF NOT EXISTS too).
	require.NoError(t, db.MigratePool(context.Background(), dbPool))
}

func TestMigratePool_TablesExist(t *testing.T) {
	expected := []string{
		"users", "active_drivers", "active_passengers",
		"ride_requests", "completed_trips", "messages",
		"planned_trips", "ratings",
	}
	for _, table := range expected {
		var present bool
		require.NoError(t, dbPool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, table).Scan(&present))
		assert.True(t, present, "table %s must exist", table)
	}
}

func TestMigratePool_PartialUniqueIndexExists(t *testing.T) {
	var present bool
	require.NoError(t, dbPool.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public'
			  AND tablename  = 'ride_requests'
			  AND indexname  = 'uniq_active_ride_request'
		)
	`).Scan(&present))
	assert.True(t, present, "partial unique index uniq_active_ride_request must exist")
}

func TestMigratePool_CurrentLocationColumnExists(t *testing.T) {
	var present bool
	require.NoError(t, dbPool.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name   = 'active_drivers'
			  AND column_name  = 'current_location'
		)
	`).Scan(&present))
	assert.True(t, present, "active_drivers.current_location column must exist")
}
