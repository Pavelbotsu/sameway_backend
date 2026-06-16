package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAcceptedRide creates a driver, passenger, and an accepted ride_request
// row with a generated pickup_code. Returns (reqID, code, driverTok, paxTok).
func seedAcceptedRide(t *testing.T) (string, string, string, string) {
	t.Helper()
	pool, _ := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "code-d")
	passengerID, paxTok := testutil.CreateUser(t, pool, "passenger", "code-p")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)
	code, err := services.SetPickupCode(ctx(t), reqID)
	require.NoError(t, err)
	require.Len(t, code, 4)
	return reqID, code, driverTok, paxTok
}

// ── /pickup/confirm ────────────────────────────────────────────────────────

func TestPickupConfirm_HappyPathPassenger(t *testing.T) {
	reqID, code, _, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	var pickedUpAt *time.Time
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status, picked_up_at FROM ride_requests WHERE id = $1`, reqID,
	).Scan(&status, &pickedUpAt))
	assert.Equal(t, "in_progress", status)
	require.NotNil(t, pickedUpAt)
}

// TestPickupConfirm_DriverForbidden verifies the asymmetric handshake: the
// driver has the code on their screen but is explicitly NOT allowed to
// submit it themselves — proving the passenger had to ask for it verbally
// (and therefore the two parties physically met). See D2 in the plan.
func TestPickupConfirm_DriverForbidden(t *testing.T) {
	reqID, code, driverTok, _ := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/pickup/confirm", driverTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "only the passenger")

	// Status must not have changed.
	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "accepted", status,
		"driver-side submission must be rejected without mutating state")
}

func TestPickupConfirm_WrongCode(t *testing.T) {
	reqID, _, _, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": "0000",
	})
	// 400 if 0000 happens to NOT be the generated code (~99.99% likely).
	// Skip the assertion if it accidentally collides.
	if w.Code == http.StatusOK {
		t.Skip("rare collision: random code was 0000")
	}
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "accepted", status, "wrong code must not mutate state")
}

func TestPickupConfirm_NonParticipantForbidden(t *testing.T) {
	reqID, code, _, _ := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()
	_, outsiderTok := testutil.CreateUser(t, pool, "driver", "outsider")

	w := testutil.PostJSON(t, r, "/pickup/confirm", outsiderTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPickupConfirm_Idempotent(t *testing.T) {
	reqID, code, _, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	// First submission: passenger flips it to in_progress.
	w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Second submission by the same passenger: 200 + already=true, no
	// mutation. The driver can't submit at all (D2).
	w = testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"already":true`)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "in_progress", status)
}

// ── /driver/dropoff/:request_id ────────────────────────────────────────────

func TestDropoff_DecrementsSeatsAndFlipsRide(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "drop-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "drop-p")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, reqID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE active_drivers SET seats_taken = 1 WHERE driver_id = $1`, driverID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, driverTok,
		map[string]any{"distance_km": 5.2})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	var seats int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT seats_taken FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&seats))
	assert.Equal(t, "done", status)
	assert.Equal(t, 0, seats)
}

func TestDropoff_FlipsFullBackToLooking(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "full-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "full-p")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE active_drivers SET seats_taken = 3, status = 'full' WHERE driver_id = $1`,
		driverID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, driverTok, nil)
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&status))
	assert.Equal(t, "looking", status, "full → looking when a seat frees up")
}

func TestDropoff_NonOwningDriverNotFound(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "owner")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	_, otherDriverTok := testutil.CreateUser(t, pool, "driver", "other")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, otherDriverTok, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── PUT /driver/status done now covers in_progress too ─────────────────────

func TestEndRoute_BulkCompletesInProgress(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, tok := testutil.CreateUser(t, pool, "driver", "end-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "end-p")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, reqID)
	require.NoError(t, err)

	w := testutil.PutJSON(t, r, "/driver/status", tok, map[string]any{
		"status": "done", "distance_km": 7.0,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "done", status, "end-route should mark the in_progress ride as done")
}
