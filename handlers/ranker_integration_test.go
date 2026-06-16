package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SetRoute without prefer_ai (or with AI unconfigured) must always tag the
// resulting ride_request WS event with match_method:"geometric".
func TestSetRoute_MatchMethodGeometricByDefault(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d-mm-geo")
	passengerID, paxTok := testutil.CreateUser(t, pool, "passenger", "p-mm-geo")
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0, Lng: 20.01}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, paxTok)
	defer conn.Close()

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "geometric", payload["match_method"],
		"default path tags as geometric — old clients ignore field, new clients label it")
}

// prefer_ai=true with no AI configured: handler must still complete and
// match_method stays "geometric" (no fallback tag — there was nothing to
// fall back from).
func TestSetRoute_PreferAIWithoutConfigStaysGeometric(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d-mm-pref")
	passengerID, paxTok := testutil.CreateUser(t, pool, "passenger", "p-mm-pref")
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0, Lng: 20.01}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, paxTok)
	defer conn.Close()

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
		"prefer_ai": true,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	// Active ranker in tests is the default GeometricRanker, so the tag
	// stays "geometric" even when the client opts in.
	assert.Equal(t, "geometric", payload["match_method"])
}

// TestSetRoute_MatchKindCorridor confirms the new match_kind field reaches
// the WS payload. A strict-corridor passenger gets tagged "corridor"; the
// destination-anchored case is exercised at the service layer in geo_test.
func TestSetRoute_MatchKindCorridor(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d-mk")
	passengerID, paxTok := testutil.CreateUser(t, pool, "passenger", "p-mk")
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0, Lng: 20.01}, nil)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, paxTok)
	defer conn.Close()

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "corridor", payload["match_kind"])
}
