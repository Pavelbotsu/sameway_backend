package db_test

import (
	"context"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigurePool(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("host=localhost user=u dbname=d")
	require.NoError(t, err)

	db.ConfigurePool(cfg, 20, 5000)
	assert.EqualValues(t, 20, cfg.MaxConns)
	assert.Equal(t, "5000", cfg.ConnConfig.RuntimeParams["statement_timeout"])
	assert.Equal(t, "force_custom_plan", cfg.ConnConfig.RuntimeParams["plan_cache_mode"])
}

// Zero values keep pgx's defaults rather than producing a 0-connection pool
// or "no timeout" by accident.
func TestConfigurePool_ZeroKeepsDefaults(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("host=localhost user=u dbname=d")
	require.NoError(t, err)
	def := cfg.MaxConns

	db.ConfigurePool(cfg, 0, 0)
	assert.Equal(t, def, cfg.MaxConns)
	_, set := cfg.ConnConfig.RuntimeParams["statement_timeout"]
	assert.False(t, set)
}

// The migration runs with the statement timeout lifted, even on a pool that
// has one: boot DDL may outlast the request timeout.
func TestMigratePool_IgnoresStatementTimeout(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(dbPool.Config().ConnString())
	require.NoError(t, err)
	db.ConfigurePool(cfg, 4, 1) // 1 ms: anything real would time out
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	defer pool.Close()
	require.NoError(t, db.MigratePool(context.Background(), pool))
}
