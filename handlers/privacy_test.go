package handlers_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Pre-match privacy (audit P0-2) ──────────────────────────────────────────
//
// Before a ride is accepted, other users must not learn a driver's exact
// start/end points (usually home and work), full name or licence plate.

func namedDriver(t *testing.T, name string) (id, tok string) {
	t.Helper()
	pool := handlersPool
	id, tok = testutil.CreateUser(t, pool, "driver", "drv")
	_, err := pool.Exec(ctx(t),
		`UPDATE users SET name = $2, car_make = 'Skoda', car_plate = 'KR 12345' WHERE id = $1`, id, name)
	require.NoError(t, err)
	return id, tok
}

func TestNearbyDrivers_RevealsOnlyCoarseDriverData(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := namedDriver(t, "Jan Kowalski")
	// Off-grid start/end so snapping is observable.
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50.0012, Lng: 20.0013},
		testutil.LatLng{Lat: 50.0012, Lng: 20.1037},
		testutil.LineStringWKT([2]float64{20.0013, 50.0012}, [2]float64{20.1037, 50.0012}),
	)
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)

	w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
		"latitude": 50.0, "longitude": 20.02, "radius_km": 10.0,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	require.Len(t, list, 1)
	d := list[0]

	assert.Equal(t, "Jan", d["driver_name"], "first name only before a match")
	assert.NotContains(t, d, "car_plate")
	// Snapped to the 0.005° grid, not the exact points.
	assert.InDelta(t, 50.0, d["origin_lat"].(float64), 1e-9)
	assert.InDelta(t, 20.0, d["origin_lng"].(float64), 1e-9)
	assert.InDelta(t, 20.105, d["dest_lng"].(float64), 1e-9)

	// The drawn route starts ~300 m in from the real origin (≈0.0042° of
	// longitude at this latitude), so its first vertex doesn't pin the address.
	wkt := d["route_wkt"].(string)
	require.True(t, strings.HasPrefix(wkt, "LINESTRING("), wkt)
	first := strings.Fields(strings.TrimPrefix(wkt, "LINESTRING("))[0]
	firstLng, err := strconv.ParseFloat(first, 64)
	require.NoError(t, err)
	assert.Greater(t, firstLng, 20.0013+0.003)
}

// The query is centred on the caller's own stored location; claiming to be
// somewhere else does not reveal that area's drivers.
func TestNearbyDrivers_IgnoresFarAwayQueryPoint(t *testing.T) {
	pool, r := setupHandlers(t)
	farID, _ := namedDriver(t, "Far Away")
	// ~33 km north of the passenger.
	testutil.CreateActiveDriver(t, pool, farID,
		testutil.LatLng{Lat: 50.3, Lng: 20.0},
		testutil.LatLng{Lat: 50.3, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20.0, 50.3}, [2]float64{20.1, 50.3}),
	)
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.0}, nil)

	w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
		"latitude": 50.3, "longitude": 20.0, "radius_km": 5.0,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	assert.Empty(t, list, "search must stay around the caller's real position")
}

func TestNearbyDrivers_RequiresStoredLocation(t *testing.T) {
	pool, r := setupHandlers(t)
	_, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
		"latitude": 50.0, "longitude": 20.0,
	})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "location_required")
}

func TestMyRequests_PlateAndFullNameOnlyAfterAccept(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := namedDriver(t, "Jan Kowalski")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	var reqID string
	require.NoError(t, pool.QueryRow(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'offered') RETURNING id`, driverID, paxID).Scan(&reqID))

	get := func() map[string]any {
		w := testutil.GetJSON(t, r, "/passenger/my-requests", paxTok)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var list []map[string]any
		testutil.DecodeJSON(t, w, &list)
		require.Len(t, list, 1)
		return list[0]
	}

	offered := get()
	assert.NotContains(t, offered, "car_plate")
	assert.Equal(t, "Jan", offered["driver_name"])

	_, err := pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'accepted', pickup_code = '1234' WHERE id = $1`, reqID)
	require.NoError(t, err)
	accepted := get()
	assert.Equal(t, "KR 12345", accepted["car_plate"])
	assert.Equal(t, "Jan Kowalski", accepted["driver_name"])
}

// Corridor offers go to many passengers at once, before anyone accepts.
func TestCorridorOffer_HasNoPlateAndCoarseDestination(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := namedDriver(t, "Jan Kowalski")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})
	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0012, "destination_lng": 20.1037,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	payload := testutil.ReadEventOfType(t, paxConn, 2*time.Second, "ride_request")
	assert.NotContains(t, payload, "car_plate")
	assert.Equal(t, "Jan", payload["driver_name"])
	assert.InDelta(t, 50.0, payload["destination_lat"].(float64), 1e-9)
	assert.InDelta(t, 20.105, payload["destination_lng"].(float64), 1e-9)
}
