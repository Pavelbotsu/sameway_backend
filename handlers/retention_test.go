package handlers_test

import (
	"testing"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Old data goes, recent data and anything still active stays.
func TestRetention_RemovesOnlyExpiredData(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "ret-d")
	paxID, _ := testutil.CreateUser(t, pool, "passenger", "ret-p")
	otherID, _ := testutil.CreateUser(t, pool, "passenger", "ret-o")
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx(t), sql, args...)
		require.NoError(t, err)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx(t), sql, args...).Scan(&n))
		return n
	}

	// Rides: one finished 4 years ago, one finished last month, one still
	// accepted but created 4 years ago (must survive: it's active).
	oldRide, _, err := services.CreateRideRequest(driverID, paxID, 50, 20)
	require.NoError(t, err)
	exec(`UPDATE ride_requests SET status = 'done', created_at = NOW() - INTERVAL '4 years' WHERE id = $1`, oldRide)
	exec(`INSERT INTO ratings (rater_id, rated_id, ride_request_id, stars) VALUES ($1, $2, $3, 5)`, paxID, driverID, oldRide)
	newRide, _, err := services.CreateRideRequest(driverID, otherID, 50, 20)
	require.NoError(t, err)
	exec(`UPDATE ride_requests SET status = 'done', created_at = NOW() - INTERVAL '30 days' WHERE id = $1`, newRide)

	// Safety flag from 3 years ago on the recent ride: flag cleared, ride kept.
	exec(`UPDATE ride_requests SET safety_flagged_at = NOW() - INTERVAL '3 years', safety_flagged_by = $2, safety_flag_reason = 'x' WHERE id = $1`, newRide, otherID)

	// Chat: one idle for 13 months, one active.
	a, b := driverID, paxID
	if a > b {
		a, b = b, a
	}
	exec(`INSERT INTO conversations (user_a, user_b, created_at, last_message_at) VALUES ($1, $2, NOW() - INTERVAL '2 years', NOW() - INTERVAL '13 months')`, a, b)
	c1, c2 := driverID, otherID
	if c1 > c2 {
		c1, c2 = c2, c1
	}
	exec(`INSERT INTO conversations (user_a, user_b, last_message_at) VALUES ($1, $2, NOW() - INTERVAL '1 day')`, c1, c2)

	// Reports, bug reports.
	exec(`INSERT INTO user_reports (reporter_id, reported_id, created_at) VALUES ($1, $2, NOW() - INTERVAL '3 years')`, paxID, driverID)
	exec(`INSERT INTO user_reports (reporter_id, reported_id) VALUES ($1, $2)`, otherID, driverID)
	exec(`INSERT INTO bug_reports (user_id, title, body, created_at) VALUES ($1, 't', 'b', NOW() - INTERVAL '400 days')`, paxID)
	exec(`INSERT INTO bug_reports (user_id, title, body) VALUES ($1, 't', 'b')`, paxID)

	// Last known location: a passenger quiet for 2 days, one active now.
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50, Lng: 20}, nil)
	exec(`UPDATE active_passengers SET updated_at = NOW() - INTERVAL '2 days' WHERE passenger_id = $1`, paxID)
	testutil.CreateActivePassenger(t, pool, otherID, testutil.LatLng{Lat: 50, Lng: 20}, nil)

	got := handlers.RunRetentionOnce(ctx(t))

	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1`, oldRide), "4-year-old ride deleted")
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM ratings WHERE ride_request_id = $1`, oldRide), "its rating goes with it")
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1`, newRide), "recent ride kept")
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM ride_requests WHERE id = $1 AND safety_flagged_at IS NOT NULL`, newRide))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM conversations`), "only the active conversation remains")
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM user_reports`))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM bug_reports`))
	assert.Equal(t, 0, count(`SELECT COUNT(*) FROM active_passengers WHERE passenger_id = $1`, paxID))
	assert.Equal(t, 1, count(`SELECT COUNT(*) FROM active_passengers WHERE passenger_id = $1`, otherID))
	assert.Equal(t, int64(1), got["ride_requests"])
	assert.Equal(t, int64(1), got["conversations"])
}

// A ride that is still going is never removed, however old.
func TestRetention_KeepsActiveRides(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "ret-active-d")
	paxID, _ := testutil.CreateUser(t, pool, "passenger", "ret-active-p")
	id, _, err := services.CreateRideRequest(driverID, paxID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'in_progress', created_at = NOW() - INTERVAL '5 years' WHERE id = $1`, id)
	require.NoError(t, err)

	handlers.RunRetentionOnce(ctx(t))

	var n int
	require.NoError(t, pool.QueryRow(ctx(t), `SELECT COUNT(*) FROM ride_requests WHERE id = $1`, id).Scan(&n))
	assert.Equal(t, 1, n)
}
