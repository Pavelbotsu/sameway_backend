package db_test

import (
	"context"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Re-running the boot migration must not rebuild the active-ride index or
// re-add the status constraint: each did a full-table operation under an
// exclusive lock on every deploy. Their object ids stay the same when they
// are left alone.
func TestMigratePool_RerunLeavesIndexAndConstraintAlone(t *testing.T) {
	ctx := context.Background()
	ids := func() (index, constraint uint32) {
		require.NoError(t, dbPool.QueryRow(ctx,
			`SELECT 'uniq_active_ride_request'::regclass::oid`).Scan(&index))
		require.NoError(t, dbPool.QueryRow(ctx,
			`SELECT oid FROM pg_constraint WHERE conname = 'ride_requests_status_check'`).Scan(&constraint))
		return
	}
	idx1, con1 := ids()
	require.NoError(t, db.MigratePool(ctx, dbPool))
	idx2, con2 := ids()
	assert.Equal(t, idx1, idx2, "unique index was rebuilt")
	assert.Equal(t, con1, con2, "status constraint was re-added")
}

// An outdated constraint (an older status set) is still upgraded.
func TestMigratePool_UpgradesOutdatedStatusCheck(t *testing.T) {
	ctx := context.Background()
	_, err := dbPool.Exec(ctx, `
		ALTER TABLE ride_requests DROP CONSTRAINT ride_requests_status_check;
		ALTER TABLE ride_requests ADD CONSTRAINT ride_requests_status_check
			CHECK (status IN ('pending','accepted','in_progress','done','declined','cancelled','offered'))`)
	require.NoError(t, err)

	require.NoError(t, db.MigratePool(ctx, dbPool))
	var def string
	require.NoError(t, dbPool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'ride_requests_status_check'`).Scan(&def))
	assert.Contains(t, def, "expired_passed")
}
