package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── corridor_stats WS event (live driver counters) ──────────────────────────

func TestSetRoute_EmitsCorridorStats(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, passengerID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil) // ahead, in ETA window
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, driverTok)
	time.Sleep(100 * time.Millisecond) // let the Hub registration land

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "corridor_stats", evtType)
	assert.EqualValues(t, 1, payload["candidates_found"])
	assert.EqualValues(t, 1, payload["new_notifications_sent"])
	assert.EqualValues(t, 1, payload["active_pending_count"], "offered row is pending a reply")
	assert.EqualValues(t, 1, payload["total_notified"])
}

func TestUpdateLocation_RerunEmitsCorridorStatsOnZeroMatches(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	wsBase := testutil.ServeWS(t, r)
	conn := testutil.DialWS(t, wsBase, driverTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// No passengers exist → the SetRoute stats event reports zeros.
	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "corridor_stats", evtType)
	assert.EqualValues(t, 0, payload["candidates_found"])

	// A location update triggers the movement re-run (first call for this
	// driver passes the throttle). Zero matches must STILL emit stats — the
	// counter has to stay honest while nothing is found.
	w = testutil.PutJSON(t, r, "/driver/location", driverTok, map[string]any{
		"lat": 50.0, "lng": 20.01,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload = testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "corridor_stats", evtType)
	assert.EqualValues(t, 0, payload["candidates_found"])
	assert.EqualValues(t, 0, payload["new_notifications_sent"])
	assert.EqualValues(t, 0, payload["active_pending_count"])
}
