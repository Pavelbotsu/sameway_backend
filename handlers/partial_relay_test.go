package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Partial relay (Round 2 §1) — topology table ─────────────────────────────
//
// Fixed geometry for every case: driver route runs west→east along lat 50.0
// from lng 20.0 to 20.1 (~7 km); the passenger stands ON the route at lng
// 20.02 with a destination at lng 20.2 — ~7 km PAST the route end. Round 1
// rejected this outright; now it becomes a partial_relay offer IFF the route
// end connects to something (transit stop within GTFS_STOP_RADIUS_M, or
// enough other looking drivers for a second leg).

const relayDestLng = 20.2

func driverRouteBody() map[string]any {
	return map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	}
}

// Destination past route end + stop within radius → offered as partial_relay
// with the stop surfaced to the passenger.
func TestPartialRelay_TransitStopWithinRadius_Offered(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	dest := testutil.LatLng{Lat: 50.0, Lng: relayDestLng}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)

	// Stop ~140 m from the route end (50.0, 20.1) — inside the 300 m default.
	_, err := pool.Exec(ctx(t), `
		INSERT INTO transit_stops (stop_id, name, location)
		VALUES ('s1', 'Rondo Testowe', ST_MakePoint(20.102, 50.0)::geography)
	`)
	require.NoError(t, err)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	require.EqualValues(t, 1, resp["notified_count"], "relay candidate must be offered")

	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "partial_relay", payload["match_kind"])
	assert.Equal(t, "Rondo Testowe", payload["transit_stop_name"])
	assert.Equal(t, false, payload["relay_fallback"])
	assert.InDelta(t, 50.0, payload["dropoff_lat"].(float64), 0.001)
	assert.InDelta(t, 20.1, payload["dropoff_lng"].(float64), 0.001)
	assert.Less(t, payload["transit_stop_dist_m"].(float64), 300.0)

	// The offer row is labeled + the outcome log started.
	var kind string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT match_kind FROM ride_requests WHERE id = $1`,
		payload["request_id"]).Scan(&kind))
	assert.Equal(t, "partial_relay", kind)
}

// Stop OUTSIDE the radius and no second-leg drivers → rejected exactly as
// before the relay branch existed.
func TestPartialRelay_NoContinuation_Rejected(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	dest := testutil.LatLng{Lat: 50.0, Lng: relayDestLng}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)

	// Stop ~640 m from the route end — outside the 300 m default radius.
	_, err := pool.Exec(ctx(t), `
		INSERT INTO transit_stops (stop_id, name, location)
		VALUES ('s-far', 'Far Stop', ST_MakePoint(20.109, 50.0)::geography)
	`)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 0, resp["notified_count"],
		"no stop in reach + no relay drivers → reject as today")
}

// No stops at all, but enough OTHER looking drivers around the dropoff →
// offered with the relay-search fallback flagged (passenger opts into
// "ride to X, then we search again from there").
func TestPartialRelay_DriverDensityFallback_Offered(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tokA := testutil.CreateUser(t, pool, "driver", "dA")
	_, tokB := testutil.CreateUser(t, pool, "driver", "dB")
	_, tokC := testutil.CreateUser(t, pool, "driver", "dC")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	// Two other drivers whose routes pass within the corridor of A's route
	// end (50.0, 20.1) — the second-leg density. Their own corridor runs
	// can't match the passenger: the passenger stands BEHIND their origins.
	w := testutil.PostJSON(t, r, "/driver/route", tokB, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.095,
		"destination_lat": 50.0, "destination_lng": 20.15,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	w = testutil.PostJSON(t, r, "/driver/route", tokC, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.098,
		"destination_lat": 49.95, "destination_lng": 20.12,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	dest := testutil.LatLng{Lat: 50.0, Lng: relayDestLng}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w = testutil.PostJSON(t, r, "/driver/route", tokA, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	require.EqualValues(t, 1, resp["notified_count"])

	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "partial_relay", payload["match_kind"])
	assert.Equal(t, true, payload["relay_fallback"], "no stop → explicit relay-search opt-in")
	assert.Equal(t, "", payload["transit_stop_name"])
}

// Leg-1 gates still apply: a relay candidate whose walk-in exceeds the cap
// is not offered even with a perfect transit connection.
func TestPartialRelay_WalkGateStillEnforced(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	dest := testutil.LatLng{Lat: 50.0, Lng: relayDestLng}
	// ~2.2 km north of the route — far beyond the 600 m walk cap.
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.02, Lng: 20.02}, &dest)

	_, err := pool.Exec(ctx(t), `
		INSERT INTO transit_stops (stop_id, name, location)
		VALUES ('s1', 'Rondo Testowe', ST_MakePoint(20.1, 50.0)::geography)
	`)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 0, resp["notified_count"], "walk cap must still reject")
}

// Full relay lifecycle: offer → passenger accept → driver accept → dropoff.
// The dropoff must fire relay_leg2_search to the passenger and the outcome
// log must contain the full labeled trail.
func TestPartialRelay_DropoffStartsLeg2AndLogsOutcomes(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	dest := testutil.LatLng{Lat: 50.0, Lng: relayDestLng}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)
	_, err := pool.Exec(ctx(t), `
		INSERT INTO transit_stops (stop_id, name, location)
		VALUES ('s1', 'Rondo Testowe', ST_MakePoint(20.1, 50.0)::geography)
	`)
	require.NoError(t, err)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	evtType, payload := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_request", evtType)
	reqID := payload["request_id"].(string)

	// Passenger opts in (they saw the relay terms on the offer card).
	w = testutil.PostJSON(t, r, "/passenger/respond", paxTok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)
	// Driver confirms — seat reserved.
	w = testutil.PostJSON(t, r, "/driver/respond", driverTok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Drain the passenger's driver_response frame before the dropoff frames.
	evtType, _ = testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "driver_response", evtType)

	// Pickup code confirmed: drop-off needs a ride that is in progress.
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, reqID)
	require.NoError(t, err)
	w = testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, driverTok, map[string]any{})
	require.Equal(t, http.StatusOK, w.Code)

	// Passenger receives ride_done then relay_leg2_search (order fixed:
	// both are emitted synchronously by the dropoff handler).
	evtType, _ = testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "ride_done", evtType)
	evtType, p2 := testutil.ReadEvent(t, paxConn, 2*time.Second)
	require.Equal(t, "relay_leg2_search", evtType)
	assert.Equal(t, reqID, p2["request_id"])

	// Outcome trail: offered, passenger_accepted, driver_accepted,
	// completed, leg2_search_started. Async writers → poll briefly.
	deadline := time.Now().Add(3 * time.Second)
	var events []string
	for time.Now().Before(deadline) {
		rows, err := pool.Query(ctx(t), `
			SELECT event FROM match_events WHERE request_id = $1 ORDER BY created_at
		`, reqID)
		require.NoError(t, err)
		events = events[:0]
		for rows.Next() {
			var e string
			require.NoError(t, rows.Scan(&e))
			events = append(events, e)
		}
		rows.Close()
		if len(events) >= 5 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assert.Contains(t, events, "offered")
	assert.Contains(t, events, "passenger_accepted")
	assert.Contains(t, events, "driver_accepted")
	assert.Contains(t, events, "completed")
	assert.Contains(t, events, "leg2_search_started")
}
