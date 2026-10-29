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

// ── /passenger/location ────────────────────────────────────────────────────

func TestPassengerUpdateLocation_PersistsRow(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")

	w := testutil.PutJSON(t, r, "/passenger/location", tok, map[string]any{
		"lat": 50.0, "lng": 20.0,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var lat, lng float64
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT ST_Y(location::geometry), ST_X(location::geometry)
		FROM active_passengers WHERE passenger_id = $1
	`, id).Scan(&lat, &lng))
	assert.Equal(t, 50.0, lat)
	assert.Equal(t, 20.0, lng)
}

// ── /passenger/nearby-drivers — self-exclusion guard ───────────────────────

func TestNearbyDrivers_ExcludesSelf(t *testing.T) {
	pool, r := setupHandlers(t)
	// User who is both a driver (active row) AND querying as a passenger.
	id, tok := testutil.CreateUser(t, pool, "passenger", "self")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	// Plus a different active driver nearby.
	otherID, _ := testutil.CreateUser(t, pool, "driver", "other")
	testutil.CreateActiveDriver(t, pool, otherID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)

	w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", tok, map[string]any{
		"latitude": 50.0, "longitude": 20.0, "radius_km": 5.0,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	require.Len(t, list, 1, "self must not appear")
	assert.Equal(t, otherID, list[0]["driver_id"])
}

// ── /passenger/request-driver ──────────────────────────────────────────────

func TestRequestDriver_RejectsSelf(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20}, nil)

	w := testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": id, // requesting yourself
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "cannot request yourself")
}

func TestRequestDriver_RequiresActiveLocation(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	// No CreateActivePassenger — passenger has no location set.

	w := testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": driverID,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "update your location")
}

func TestRequestDriver_HappyPath(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	// Mechanism B projects the pickup onto the driver's route, so the driver
	// must have one. Passenger sits ~55 m off and ~1.4 km ahead of the origin.
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0005, Lng: 20.02}, nil)

	w := testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Row created with status pending.
	var n int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM ride_requests
		 WHERE driver_id = $1 AND passenger_id = $2 AND status = 'pending'`,
		driverID, passengerID).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestRequestDriver_IdempotentOnExistingRow guards the regression we just
// fixed: tapping Request a second time on the same driver must NOT push a
// duplicate `passenger_request` WS to the driver, and must not mutate the row.
func TestRequestDriver_IdempotentOnExistingRow(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0005, Lng: 20.02}, nil)

	// Driver opens a WS connection so we can assert NO duplicate event arrives.
	wsBase := testutil.ServeWS(t, r)
	_, driverTok := func() (string, string) {
		// We need the driver's token — fish it back out via login. The driver
		// user was created by testutil.CreateUser which used "test-pass".
		w := testutil.PostJSON(t, r, "/auth/login", "", map[string]any{
			"email": "d1@test.local", "password": "test-pass",
		})
		require.Equal(t, http.StatusOK, w.Code)
		var body map[string]any
		testutil.DecodeJSON(t, w, &body)
		return body["user_id"].(string), body["token"].(string)
	}()
	conn := testutil.DialWS(t, wsBase, driverTok)

	// First request → expect WS event for the driver.
	w1 := testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": driverID,
	})
	require.Equal(t, http.StatusOK, w1.Code)
	evtType, _ := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "passenger_request", evtType)

	// Second request → must NOT push a duplicate WS event.
	w2 := testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": driverID,
	})
	require.Equal(t, http.StatusOK, w2.Code)
	testutil.ExpectNoEvent(t, conn, 500*time.Millisecond)

	// And exactly one active row exists.
	var n int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM ride_requests
		 WHERE driver_id = $1 AND passenger_id = $2 AND status = 'pending'`,
		driverID, passengerID).Scan(&n))
	assert.Equal(t, 1, n, "no duplicate row")
}

// ── /passenger/search ──────────────────────────────────────────────────────

func TestPassengerSearch_PersistsDestination(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{}) // straight-line fallback

	w := testutil.PostJSON(t, r, "/passenger/search", tok, map[string]any{
		"dest_lat": 50.01, "dest_lng": 20.05,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var destLat, destLng float64
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT destination_lat, destination_lng FROM active_passengers WHERE passenger_id = $1`,
		id).Scan(&destLat, &destLng))
	assert.Equal(t, 50.01, destLat)
	assert.Equal(t, 20.05, destLng)
}

func TestPassengerSearch_RequiresLocation(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	w := testutil.PostJSON(t, r, "/passenger/search", tok, map[string]any{
		"dest_lat": 50.01, "dest_lng": 20.05,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── /passenger/my-requests ─────────────────────────────────────────────────

func TestGetMyRequests_OnlyActive(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")

	// One pending (active) + one cancelled (history).
	_, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status)
		 VALUES ($1, $2, ST_MakePoint(20, 50)::geography, 'cancelled')`,
		driverID, passengerID)
	require.NoError(t, err)

	w := testutil.GetJSON(t, r, "/passenger/my-requests", tok)
	require.Equal(t, http.StatusOK, w.Code)
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	require.Len(t, list, 1, "only pending+accepted returned")
}

// ── /passenger/request DELETE (cancel pending) ─────────────────────────────

func TestPassengerCancelPendingRequest_FlipsStatus(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	id, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.DeleteJSON(t, r, "/passenger/request/"+id, tok)
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "cancelled", status)
}

// ── /passenger/respond (handshake: accept an offer) ────────────────────────

// Accepting a driver-broadcast offer must NOT finalize — it promotes the row to
// 'pending' (awaiting the driver's confirm) and reserves no seat.
func TestRespondAccept_PromotesOfferedToPending(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	// A driver-broadcast offer with the pickup ~1.4 km ahead.
	reqID, _, err := services.CreateRideRequestWithStatus(
		driverID, passengerID, 50, 20.02, "offered")
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/passenger/respond", tok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	var seats int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT seats_taken FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&seats))
	assert.Equal(t, "pending", status, "accept only requests; driver must confirm")
	assert.Equal(t, 0, seats, "no seat reserved on passenger accept")
}

// ── /passenger/match-advice ────────────────────────────────────────────────

func TestMatchAdvice_TripTooShort(t *testing.T) {
	pool, r := setupHandlers(t)
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	// Destination ~140 m away → under the 1 km short-trip threshold.
	dest := testutil.LatLng{Lat: 50.0, Lng: 20.002}
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0, Lng: 20.0}, &dest)

	w := testutil.PostJSON(t, r, "/passenger/match-advice", tok, map[string]any{})
	require.Equal(t, http.StatusOK, w.Code)
	var advice map[string]any
	testutil.DecodeJSON(t, w, &advice)
	assert.Equal(t, true, advice["trip_too_short"])
	assert.Contains(t, advice["suggestions"], "trip_too_short")
}

// ── /passenger/active DELETE (go offline) ──────────────────────────────────

func TestGoOffline_DeletesActiveRow(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20}, nil)

	w := testutil.DeleteJSON(t, r, "/passenger/active", tok)
	require.Equal(t, http.StatusOK, w.Code)

	var n int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM active_passengers WHERE passenger_id = $1`, id).Scan(&n))
	assert.Equal(t, 0, n)
}

// ── reaper smoke (proves the singleton goroutine isn't required for unit tests) ──

func TestReapPendingRideRequests_ManualRun(t *testing.T) {
	pool, _ := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	// The reaper ages rows off status_changed_at (how long the row has sat in
	// its CURRENT status) rather than created_at, so that a passenger's accept
	// gives the driver a fresh confirmation window. Backdate both.
	_, err = pool.Exec(ctx(t), `
		UPDATE ride_requests
		SET created_at = NOW() - INTERVAL '20 minutes',
		    status_changed_at = NOW() - INTERVAL '20 minutes'
		WHERE id = $1`, id)
	require.NoError(t, err)

	// Call the reaping helper directly (the goroutine version is plumbed in main).
	handlers.ReapStalePendingOnce(5)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, id).Scan(&status))
	assert.Equal(t, "cancelled", status)
}
