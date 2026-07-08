package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the two seat-accounting leaks: capacity must be
// restored whenever an 'accepted' row leaves the active set by any path.

// seatState reads (seats_taken, status) for a driver.
func seatState(t *testing.T, driverID string) (int, string) {
	t.Helper()
	pool := handlersPool
	var taken int
	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT seats_taken, status FROM active_drivers WHERE driver_id = $1`,
		driverID).Scan(&taken, &status))
	return taken, status
}

func rideStatus(t *testing.T, requestID string) string {
	t.Helper()
	var s string
	require.NoError(t, handlersPool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, requestID).Scan(&s))
	return s
}

// A passenger accepted by driver A is then accepted by driver B. A's row is
// auto-cancelled — and A's seat MUST come back (previously it leaked until A
// reset their route).
func TestDriverAccept_RefundsBumpedDriverSeat(t *testing.T) {
	pool, r := setupHandlers(t)
	driverA, tokA := testutil.CreateUser(t, pool, "driver", "dA")
	driverB, tokB := testutil.CreateUser(t, pool, "driver", "dB")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	route := testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.00, 50.10})
	testutil.CreateActiveDriver(t, pool, driverA,
		testutil.LatLng{Lat: 50.00, Lng: 20.00}, testutil.LatLng{Lat: 50.10, Lng: 20.00}, route)
	testutil.CreateActiveDriver(t, pool, driverB,
		testutil.LatLng{Lat: 50.00, Lng: 20.00}, testutil.LatLng{Lat: 50.10, Lng: 20.00}, route)

	// Pickup well ahead of both drivers' current position (origin).
	rideA, _, err := services.CreateRideRequest(driverA, passengerID, 50.05, 20.00)
	require.NoError(t, err)
	w := testutil.PostJSON(t, r, "/driver/respond", tokA,
		map[string]any{"request_id": rideA, "status": "accepted"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	taken, _ := seatState(t, driverA)
	require.Equal(t, 1, taken, "driver A reserved a seat")

	rideB, _, err := services.CreateRideRequest(driverB, passengerID, 50.05, 20.00)
	require.NoError(t, err)
	w = testutil.PostJSON(t, r, "/driver/respond", tokB,
		map[string]any{"request_id": rideB, "status": "accepted"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// B holds the passenger now; A's row is cancelled AND A's seat refunded.
	assert.Equal(t, "cancelled", rideStatus(t, rideA))
	assert.Equal(t, "accepted", rideStatus(t, rideB))
	takenA, statusA := seatState(t, driverA)
	assert.Equal(t, 0, takenA, "bumped driver's seat must be refunded")
	assert.Equal(t, "looking", statusA)
	takenB, _ := seatState(t, driverB)
	assert.Equal(t, 1, takenB)
}

// Declining is only legal from pre-seat states. An accepted ride must go
// through the cancel endpoint (which restores capacity) — previously a
// decline flipped the row without a seat refund.
func TestPassengerDecline_RejectedOnAcceptedRide(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, dTok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, pTok := testutil.CreateUser(t, pool, "passenger", "p1")

	route := testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.00, 50.10})
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50.00, Lng: 20.00}, testutil.LatLng{Lat: 50.10, Lng: 20.00}, route)

	rideID, _, err := services.CreateRideRequest(driverID, passengerID, 50.05, 20.00)
	require.NoError(t, err)
	w := testutil.PostJSON(t, r, "/driver/respond", dTok,
		map[string]any{"request_id": rideID, "status": "accepted"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/passenger/respond", pTok,
		map[string]any{"request_id": rideID, "status": "declined"})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// Ride untouched, seat still reserved.
	assert.Equal(t, "accepted", rideStatus(t, rideID))
	taken, _ := seatState(t, driverID)
	assert.Equal(t, 1, taken)
}

// An accepted ride must always carry a pickup code — it is now written inside
// the accept transaction (previously post-commit with a swallowed error,
// which could leave a ride that ConfirmPickup could never complete).
func TestDriverAccept_AlwaysStampsPickupCode(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, dTok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	route := testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.00, 50.10})
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50.00, Lng: 20.00}, testutil.LatLng{Lat: 50.10, Lng: 20.00}, route)

	rideID, _, err := services.CreateRideRequest(driverID, passengerID, 50.05, 20.00)
	require.NoError(t, err)
	w := testutil.PostJSON(t, r, "/driver/respond", dTok,
		map[string]any{"request_id": rideID, "status": "accepted"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var code *string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT pickup_code FROM ride_requests WHERE id = $1`, rideID).Scan(&code))
	require.NotNil(t, code)
	assert.Len(t, *code, 4)

	// The accept response hands the code to the driver directly.
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	assert.Equal(t, *code, body["pickup_code"])
}
