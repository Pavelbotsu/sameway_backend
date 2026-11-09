package handlers_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Recovery for accepted rides that can't proceed (audit P1-7) ─────────────

func wrongCodeFor(code string) string {
	if code == "0000" {
		return "1111"
	}
	return "0000"
}

func TestPickup_LockoutThenDriverRegeneratesCode(t *testing.T) {
	reqID, code, driverTok, paxTok := seedAcceptedRide(t)
	r := testutil.NewTestRouter()
	submit := func(c string) (int, map[string]any) {
		w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{"request_id": reqID, "code": c})
		var body map[string]any
		testutil.DecodeJSON(t, w, &body)
		return w.Code, body
	}

	// Each wrong code says how many tries remain.
	for left := 4; left >= 1; left-- {
		status, body := submit(wrongCodeFor(code))
		require.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "wrong_code", body["code"])
		assert.EqualValues(t, left, body["attempts_left"])
	}
	status, body := submit(wrongCodeFor(code))
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, "pickup_locked", body["code"])

	// Locked means locked: even the right code doesn't start the ride now.
	status, _ = submit(code)
	assert.Equal(t, http.StatusTooManyRequests, status)

	// The driver issues a new code; the passenger can finish the pickup.
	w := testutil.PostJSON(t, r, "/driver/pickup-code/"+reqID+"/regenerate", driverTok, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var regen map[string]any
	testutil.DecodeJSON(t, w, &regen)
	newCode := regen["pickup_code"].(string)
	require.Len(t, newCode, 4)

	status, _ = submit(newCode)
	assert.Equal(t, http.StatusOK, status)
}

func TestPickup_RegenerateOnlyByTheRidesDriver(t *testing.T) {
	reqID, _, _, paxTok := seedAcceptedRide(t)
	r := testutil.NewTestRouter()
	otherID, otherTok := testutil.CreateUser(t, handlersPool, "driver", "other-d")
	_ = otherID
	assert.Equal(t, http.StatusNotFound,
		testutil.PostJSON(t, r, "/driver/pickup-code/"+reqID+"/regenerate", otherTok, nil).Code)
	// A passenger token can't reach the driver group at all.
	assert.Equal(t, http.StatusForbidden,
		testutil.PostJSON(t, r, "/driver/pickup-code/"+reqID+"/regenerate", paxTok, nil).Code)
}

// seatHeld puts the ride's driver on an active route with one seat taken.
func seatHeld(t *testing.T, reqID string) (driverID string) {
	t.Helper()
	require.NoError(t, handlersPool.QueryRow(ctx(t),
		`SELECT driver_id FROM ride_requests WHERE id = $1`, reqID).Scan(&driverID))
	testutil.CreateActiveDriver(t, handlersPool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20}, testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}))
	_, err := handlersPool.Exec(ctx(t),
		`UPDATE active_drivers SET seats = 1, seats_taken = 1, status = 'full' WHERE driver_id = $1`, driverID)
	require.NoError(t, err)
	return driverID
}

func rideState(t *testing.T, reqID, driverID string) (status string, reason *string, seatsTaken int, driverStatus string) {
	t.Helper()
	require.NoError(t, handlersPool.QueryRow(ctx(t),
		`SELECT status, cancel_reason FROM ride_requests WHERE id = $1`, reqID).Scan(&status, &reason))
	require.NoError(t, handlersPool.QueryRow(ctx(t),
		`SELECT seats_taken, status FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&seatsTaken, &driverStatus))
	return
}

func TestNoShow_OnlyAfterWaitThenReleasesSeat(t *testing.T) {
	reqID, _, driverTok, _ := seedAcceptedRide(t)
	r := testutil.NewTestRouter()
	driverID := seatHeld(t, reqID)

	w := testutil.PostJSON(t, r, "/driver/no-show/"+reqID, driverTok, nil)
	assert.Equal(t, http.StatusConflict, w.Code, "too early")
	assert.Contains(t, w.Body.String(), "no_show_too_early")

	// 10 minutes into the wait (default threshold 5).
	_, err := handlersPool.Exec(ctx(t),
		`UPDATE ride_requests SET status_changed_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, reqID)
	require.NoError(t, err)
	w = testutil.PostJSON(t, r, "/driver/no-show/"+reqID, driverTok, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	status, reason, taken, dstatus := rideState(t, reqID, driverID)
	assert.Equal(t, "cancelled", status)
	require.NotNil(t, reason)
	assert.Equal(t, "no_show", *reason)
	assert.Equal(t, 0, taken)
	assert.Equal(t, "looking", dstatus, "a full car has room again")
}

func TestReaper_ExpiresForgottenAcceptedRide(t *testing.T) {
	reqID, _, _, _ := seedAcceptedRide(t)
	driverID := seatHeld(t, reqID)

	// Fresh: untouched.
	handlers.ExpireStaleAcceptedOnce(context.Background())
	status, _, _, _ := rideState(t, reqID, driverID)
	require.Equal(t, "accepted", status)

	_, err := handlersPool.Exec(ctx(t),
		`UPDATE ride_requests SET status_changed_at = NOW() - INTERVAL '2 hours' WHERE id = $1`, reqID)
	require.NoError(t, err)
	handlers.ExpireStaleAcceptedOnce(context.Background())

	status, reason, taken, dstatus := rideState(t, reqID, driverID)
	assert.Equal(t, "cancelled", status)
	require.NotNil(t, reason)
	assert.Equal(t, "expired", *reason)
	assert.Equal(t, 0, taken)
	assert.Equal(t, "looking", dstatus)
}

// A ride already in the car is never expired by the timeout.
func TestReaper_KeepsRideInProgress(t *testing.T) {
	reqID, _, _, _ := seedAcceptedRide(t)
	_, err := handlersPool.Exec(ctx(t), `
		UPDATE ride_requests SET status = 'in_progress', picked_up_at = NOW() WHERE id = $1;`, reqID)
	require.NoError(t, err)
	_, err = handlersPool.Exec(ctx(t),
		`UPDATE ride_requests SET status_changed_at = NOW() - INTERVAL '5 hours' WHERE id = $1`, reqID)
	require.NoError(t, err)
	handlers.ExpireStaleAcceptedOnce(context.Background())
	var status string
	require.NoError(t, handlersPool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "in_progress", status)
}
