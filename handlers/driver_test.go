package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── /driver/route ──────────────────────────────────────────────────────────

func TestSetRoute_PersistsActiveDriver(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "d1")
	testutil.SwapRouter(t, &testutil.MockOSRM{}) // straight-line fallback

	w := testutil.PostJSON(t, r, "/driver/route", tok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM active_drivers WHERE driver_id = $1`, id).Scan(&status))
	assert.Equal(t, "looking", status)
}

func TestSetRoute_NotifiesPassengerAhead(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, passengerID,
		// 700 m ahead along the route
		testutil.LatLng{Lat: 50.0, Lng: 20.01}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", tok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 1, resp["notified_count"], "passenger ahead should be matched")
}

func TestSetRoute_SkipsPassengerBehind(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, passengerID,
		// 700 m behind the driver
		testutil.LatLng{Lat: 50.0, Lng: 19.99}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", tok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 0, resp["notified_count"], "passenger behind must be skipped")
}

// ── /driver/requests ───────────────────────────────────────────────────────

func TestGetRequests_OnlyActiveWithName(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "Alice")

	_, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status)
		 VALUES ($1, $2, ST_MakePoint(20, 50)::geography, 'cancelled')`,
		driverID, passengerID)
	require.NoError(t, err)

	w := testutil.GetJSON(t, r, "/driver/requests", tok)
	require.Equal(t, http.StatusOK, w.Code)
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	require.Len(t, list, 1)
	assert.Equal(t, "Alice", list[0]["passenger_name"])
}

// ── /driver/location ───────────────────────────────────────────────────────

func TestUpdateLocation_PersistsToPostGIS(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "d1")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)

	w := testutil.PutJSON(t, r, "/driver/location", tok, map[string]any{
		"lat": 50.0, "lng": 20.05,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var lat, lng float64
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT ST_Y(current_location::geometry), ST_X(current_location::geometry)
		FROM active_drivers WHERE driver_id = $1
	`, id).Scan(&lat, &lng))
	assert.InDelta(t, 50.0, lat, 0.0001)
	assert.InDelta(t, 20.05, lng, 0.0001)
}

// ── /driver/respond ────────────────────────────────────────────────────────

func TestDriverRespondToRide_AcceptReservesSeat(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/respond", tok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	var seatsTaken int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT seats_taken FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&seatsTaken))
	assert.Equal(t, "accepted", status)
	assert.Equal(t, 1, seatsTaken)
}

func TestDriverRespondToRide_Decline(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/respond", tok, map[string]any{
		"request_id": reqID, "status": "declined",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "declined", status)
}

// ── /driver/route DELETE ───────────────────────────────────────────────────

func TestDeleteRoute_ClearsActiveDriver(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "driver", "d1")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)

	w := testutil.DeleteJSON(t, r, "/driver/route", tok)
	require.Equal(t, http.StatusOK, w.Code)

	var n int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT count(*) FROM active_drivers WHERE driver_id = $1`, id).Scan(&n))
	assert.Equal(t, 0, n)
}

// ── /driver/pickup-route/:request_id ──────────────────────────────────────

func TestGetPickupRoute_ReturnsBothLegs(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, tok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50, Lng: 20.05}, nil)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.GetJSON(t, r, "/driver/pickup-route/"+reqID, tok)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.NotEmpty(t, resp["pickup_wkt"])
	assert.NotEmpty(t, resp["continuation_wkt"])
}
