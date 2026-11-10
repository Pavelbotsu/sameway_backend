package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/handlers"
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
		`UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, reqID)
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

// The 4-digit code is the only proof the two parties physically met, so a
// guess loop must not be able to walk the 10^4 space.
func TestPickupConfirm_BruteForceCapped(t *testing.T) {
	reqID, code, _, paxTok := seedAcceptedRide(t)
	r := testutil.NewTestRouter()

	wrong := "0000"
	if wrong == code {
		wrong = "1111"
	}
	for i := 0; i < 4; i++ {
		w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
			"request_id": reqID, "code": wrong,
		})
		require.Equal(t, http.StatusBadRequest, w.Code, "attempt %d", i+1)
	}
	// The 5th wrong guess uses up the last attempt and says so straight away.
	w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": wrong,
	})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// Cap reached: further guesses are refused outright...
	w = testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": wrong,
	})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	// ...and crucially the CORRECT code is refused too, so an attacker who
	// happens to hit it on a later guess still gains nothing.
	w = testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// A handful of typos must not lock a genuine passenger out.
func TestPickupConfirm_WrongThenRightUnderCap(t *testing.T) {
	reqID, code, _, paxTok := seedAcceptedRide(t)
	r := testutil.NewTestRouter()

	wrong := "0000"
	if wrong == code {
		wrong = "1111"
	}
	for i := 0; i < 3; i++ {
		testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
			"request_id": reqID, "code": wrong,
		})
	}
	w := testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	assert.Equal(t, http.StatusOK, w.Code)
}

// The offer window and the driver-confirmation window must be independent.
//
// Rows are created 'offered' and promoted to 'pending' when the passenger
// accepts, which is when the driver's window starts. Ageing both off created_at
// shared one budget between them: a passenger deliberating for most of the
// offer timeout left the driver only the remainder to confirm, and the reaper
// cancelled the row out from under them.
func TestReaper_PendingGetsFreshWindowAfterAccept(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "reap-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "reap-p")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	// An offer created well outside the timeout, which the passenger has just
	// now accepted (status flips to 'pending' → trigger stamps status_changed_at).
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET created_at = NOW() - INTERVAL '30 minutes' WHERE id = $1`, reqID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'pending' WHERE id = $1`, reqID)
	require.NoError(t, err)

	handlers.ReapStalePendingOnce(5)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "pending", status,
		"a just-accepted request must keep its full driver-confirm window")
}

// The flip side: a row that has genuinely sat in one status past the timeout
// still gets reaped.
func TestReaper_StalePendingStillExpires(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "reap2-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "reap2-p")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	_, err = pool.Exec(ctx(t), `
		UPDATE ride_requests
		SET status = 'pending', status_changed_at = NOW() - INTERVAL '30 minutes'
		WHERE id = $1`, reqID)
	require.NoError(t, err)

	handlers.ReapStalePendingOnce(5)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "cancelled", status)
}

// No pickup code confirmed → no proof the two met → no completed ride (it
// would unlock ratings and post-ride chat).
func TestDropoff_RefusedBeforePickup(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "np-d")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "np-p")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, driverTok, nil)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "not_picked_up")

	var status string
	require.NoError(t, pool.QueryRow(ctx(t), `SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "accepted", status)
}

// Ending the day completes rides that were picked up and cancels the ones
// that never were.
func TestEndDay_CompletesPickedUpCancelsTheRest(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "ed-d")
	p1, _ := testutil.CreateUser(t, pool, "passenger", "ed-p1")
	p2, p2Tok := testutil.CreateUser(t, pool, "passenger", "ed-p2")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20}, testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}))
	rideIn, _, err := services.CreateRideRequest(driverID, p1, 50, 20)
	require.NoError(t, err)
	rideWait, _, err := services.CreateRideRequest(driverID, p2, 50, 20.01)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, rideIn)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, rideWait)
	require.NoError(t, err)

	waiting := testutil.DialWS(t, testutil.ServeWS(t, r), p2Tok)
	w := testutil.PutJSON(t, r, "/driver/status", driverTok, map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	statusOf := func(id string) (s string, reason *string) {
		require.NoError(t, pool.QueryRow(ctx(t),
			`SELECT status, cancel_reason FROM ride_requests WHERE id = $1`, id).Scan(&s, &reason))
		return
	}
	s, _ := statusOf(rideIn)
	assert.Equal(t, "done", s)
	s, reason := statusOf(rideWait)
	assert.Equal(t, "cancelled", s)
	require.NotNil(t, reason)
	assert.Equal(t, "driver_ended", *reason)

	payload := testutil.ReadEventOfType(t, waiting, 2*time.Second, "ride_cancelled")
	assert.Equal(t, "driver_ended", payload["cancelled_by"])
}
