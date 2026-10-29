package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── likely_match on the nearby-drivers list (manual map pick, Mechanism B) ──

// The passenger heads the same way as driver A (forward pickup + forward
// dropoff + enough carry) and the OPPOSITE way relative to driver B. The
// nearby list must flag A and not B, so the manual pick isn't blind.
func TestNearbyDrivers_LikelyMatchFlag(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tokA := testutil.CreateUser(t, pool, "driver", "dA")
	_, tokB := testutil.CreateUser(t, pool, "driver", "dB")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	// Driver A: west→east, the passenger's direction.
	w := testutil.PostJSON(t, r, "/driver/route", tokA, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	// Driver B: east→west, opposite direction on the same street.
	w = testutil.PostJSON(t, r, "/driver/route", tokB, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.1,
		"destination_lat": 50.0, "destination_lng": 20.0,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)

	w = testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
		"latitude": 50.0, "longitude": 20.02, "radius_km": 15.0,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var drivers []map[string]any
	testutil.DecodeJSON(t, w, &drivers)
	require.Len(t, drivers, 2)

	byOriginLng := map[float64]map[string]any{}
	for _, d := range drivers {
		byOriginLng[d["origin_lng"].(float64)] = d
	}
	sameWay, ok := byOriginLng[20.0]
	require.True(t, ok, "driver A present")
	oppositeWay, ok := byOriginLng[20.1]
	require.True(t, ok, "driver B present")

	assert.Equal(t, true, sameWay["likely_match"], "same-way driver must be flagged")
	assert.InDelta(t, 5000, sameWay["carry_m"].(float64), 100, "carry ≈ pickup→dropoff along route")
	assert.Less(t, sameWay["walk_m"].(float64), 50.0, "passenger sits on the route")

	assert.Equal(t, false, oppositeWay["likely_match"], "opposite-way driver must not be flagged")
}

// No declared destination → the hint stays false (unknown ≠ likely).
func TestNearbyDrivers_NoDestinationNoLikelyMatch(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tokA := testutil.CreateUser(t, pool, "driver", "dA")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", tokA, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)

	w = testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
		"latitude": 50.0, "longitude": 20.02, "radius_km": 15.0,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var drivers []map[string]any
	testutil.DecodeJSON(t, w, &drivers)
	require.Len(t, drivers, 1)
	assert.Equal(t, false, drivers[0]["likely_match"])
	assert.Less(t, drivers[0]["walk_m"].(float64), 50.0, "walk still computed from location alone")
}
