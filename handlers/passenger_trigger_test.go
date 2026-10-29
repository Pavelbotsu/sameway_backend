package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Reverse trigger: passenger appears after the driver is stationary ───────

// A driver sets a route and then never moves (parked). A passenger who shows
// up afterwards used to be invisible until the driver's next location update.
// The reverse trigger must deliver the corridor offer from the passenger's
// own location ping.
func TestPassengerLocation_TriggersReverseMatch(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	_, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	// Driver route set BEFORE the passenger exists — zero matches.
	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	require.EqualValues(t, 0, resp["notified_count"], "no passengers yet")

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	// Passenger appears ahead on the route, inside the ETA window.
	w = testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.0, "lng": 20.02,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType, "reverse trigger must deliver the offer")
	assert.Equal(t, "corridor_weak", payload["match_kind"], "no destination declared yet")
}

// Setting a destination via /passenger/search must also trigger the reverse
// check (matching inputs changed without the passenger moving) and produce a
// full-confidence corridor offer.
func TestPassengerSearch_TriggersReverseMatch(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Seed the location row directly (no HTTP ping → no trigger yet).
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w = testutil.PostJSON(t, r, "/passenger/search", paxTok, map[string]any{
		"dest_lat": 50.0, "dest_lng": 20.09, // same way, ~5 km carry
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "corridor", payload["match_kind"], "destination declared → full corridor match")
}

// Rapid successive passenger pings must not re-scan: after declining the
// first offer (freeing the pair's active slot, so a re-run WOULD create a new
// one), an immediate second ping stays inside the time floor → no new offer.
func TestPassengerTrigger_ThrottlesRapidUpdates(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	_, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w = testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.0, "lng": 20.02,
	})
	require.Equal(t, http.StatusOK, w.Code)
	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)

	// Decline → the declined row leaves the active set, so an unthrottled
	// re-run would mint a fresh offer.
	w = testutil.PostJSON(t, r, "/passenger/respond", paxTok, map[string]any{
		"request_id": payload["request_id"], "status": "declined",
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Second ping 1 m away, immediately: inside the 5 s time floor.
	w = testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.00001, "lng": 20.02,
	})
	require.Equal(t, http.StatusOK, w.Code)
	testutil.ExpectNoEvent(t, paxConn, 500*time.Millisecond)
}
