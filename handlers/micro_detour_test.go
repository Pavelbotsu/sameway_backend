package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Micro-detour (Round 2 §2) — topology + caps + budget ────────────────────
//
// Geometry shared by every case: route west→east along lat 50.0 from lng 20.0
// to 20.1. A "near-miss" passenger stands ~700 m NORTH of the route (over the
// 600 m walk cap, under the 1500 m near-miss bound) with an on-route
// destination — i.e. they fail corridor matching ONLY on walk distance.
// MockOSRM's straight-line legs give the detour ~190-220 m / ~25 s, inside
// the 300 m / 90 s per-candidate caps.

func enableMicroDetour(t *testing.T, pool *pgxpool.Pool, driverID string) {
	t.Helper()
	_, err := pool.Exec(ctx(t),
		`UPDATE users SET allow_micro_detour = TRUE WHERE id = $1`, driverID)
	require.NoError(t, err)
}

func countMicroDetourOffers(t *testing.T, pool *pgxpool.Pool, driverID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT COUNT(*) FROM ride_requests
		WHERE driver_id = $1 AND match_kind = 'micro_detour' AND status = 'offered'
	`, driverID).Scan(&n))
	return n
}

func TestMicroDetour_NearMissOfferedWithDisclosedDeltas(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0064, Lng: 20.02}, &dest) // ~712 m off-route

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 0, resp["notified_count"],
		"walk cap keeps the candidate OUT of the normal corridor result")

	// The async detour pass delivers the offer with the delta disclosed.
	evtType, payload := testutil.ReadEvent(t, paxConn, 3*time.Second)
	require.Equal(t, "ride_request", evtType)
	assert.Equal(t, "micro_detour", payload["match_kind"])
	assert.EqualValues(t, 0, payload["walk_distance_m"], "driver comes to the passenger")
	assert.InDelta(t, 50.0064, payload["pickup_lat"].(float64), 0.0005)
	assert.InDelta(t, 20.02, payload["pickup_lng"].(float64), 0.0005)
	addM := payload["detour_added_m"].(float64)
	addS := payload["detour_added_s"].(float64)
	assert.Greater(t, addM, 50.0)
	assert.LessOrEqual(t, addM, config.C.MatchMaxDetourM)
	assert.Greater(t, addS, 5.0)
	assert.LessOrEqual(t, addS, config.C.MatchMaxDetourS)

	// Persisted on the row for the driver's request tile.
	var kind string
	var dbM, dbS float64
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT match_kind, detour_added_m, detour_added_s
		FROM ride_requests WHERE id = $1`, payload["request_id"],
	).Scan(&kind, &dbM, &dbS))
	assert.Equal(t, "micro_detour", kind)
	assert.InDelta(t, addM, dbM, 0.01)
	assert.InDelta(t, addS, dbS, 0.01)
}

func TestMicroDetour_OptOutNeverEvaluates(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	// allow_micro_detour stays FALSE (the default).

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0064, Lng: 20.02}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	testutil.ExpectNoEvent(t, paxConn, 700*time.Millisecond)
	assert.Zero(t, mock.ViaCallCount(), "opt-out must not spend OSRM calls")
}

func TestMicroDetour_DistanceCapExceeded(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	// ~1330 m off-route: still a near-miss (< 1500 m) but the straight-line
	// detour comes to ~680 m added — over the 300 m cap.
	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.012, Lng: 20.02}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	testutil.ExpectNoEvent(t, paxConn, 700*time.Millisecond)
	assert.Equal(t, 1, mock.ViaCallCount(), "candidate was evaluated, then rejected")
	assert.Zero(t, countMicroDetourOffers(t, pool, driverID))
}

func TestMicroDetour_TimeCapExceeded(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	// Distance inside the cap, duration ~132 s over baseline — time cap must
	// reject on its own (BOTH caps are required).
	mock := &testutil.MockOSRM{ViaResult: &services.RouteResult{
		DistanceKm: 7.2, DurationS: 990,
	}}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0064, Lng: 20.02}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	testutil.ExpectNoEvent(t, paxConn, 700*time.Millisecond)
	assert.Zero(t, countMicroDetourOffers(t, pool, driverID))
}

// An existing LIVE micro_detour offer consumes the route budget: a second
// candidate that passes the per-candidate caps must still be refused when
// the cumulative total would exceed MatchMaxDetourTotalM/S.
func TestMicroDetour_BudgetConsumedByLiveOffer(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	otherPaxID, _ := testutil.CreateUser(t, pool, "passenger", "p2")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	// Live offer already holding 500 m / 100 s of the 600 m / 180 s budget.
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status, match_kind,
		                           detour_added_m, detour_added_s)
		VALUES ($1, $2, 'offered', 'micro_detour', 500, 100)
	`, driverID, otherPaxID)
	require.NoError(t, err)

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0064, Lng: 20.02}, &dest) // delta ≈ 190 m — fits alone

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	testutil.ExpectNoEvent(t, paxConn, 700*time.Millisecond)
	assert.Equal(t, 1, countMicroDetourOffers(t, pool, driverID),
		"only the pre-existing offer — budget refused the new one")
}

// Sequential in-pass accounting: three candidates, each individually inside
// the per-candidate caps, but with the cumulative budget tightened so only
// the FIRST fits. A batch snapshot (sum computed once for the whole pass)
// would offer all three; the running total must stop after one.
func TestMicroDetour_SequentialInPassBudget(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	p1, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	p2, _ := testutil.CreateUser(t, pool, "passenger", "p2")
	p3, _ := testutil.CreateUser(t, pool, "passenger", "p3")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	// Deltas ≈ 212 m (p1, closest ETA), ≈ 166 m (p2), ≈ 146 m (p3): each is
	// far under the 300 m per-candidate cap, and each ALONE is under the
	// tightened 300 m total — only sequential accounting stops p2/p3.
	prevTotal := config.C.MatchMaxDetourTotalM
	config.C.MatchMaxDetourTotalM = 300
	t.Cleanup(func() { config.C.MatchMaxDetourTotalM = prevTotal })

	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, p1,
		testutil.LatLng{Lat: 50.0064, Lng: 20.02}, &dest)
	testutil.CreateActivePassenger(t, pool, p2,
		testutil.LatLng{Lat: 49.9936, Lng: 20.03}, &dest)
	testutil.CreateActivePassenger(t, pool, p3,
		testutil.LatLng{Lat: 50.0064, Lng: 20.04}, &dest)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	// Wait for the async pass to finish: all three candidates get evaluated
	// (three via calls), then give the writes a beat to land.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mock.ViaCallCount() < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 3, mock.ViaCallCount(), "every candidate is evaluated")
	time.Sleep(300 * time.Millisecond)

	assert.Equal(t, 1, countMicroDetourOffers(t, pool, driverID),
		"running total must stop offers after the first — not a batch snapshot")
	var offeredTo string
	require.NoError(t, pool.QueryRow(ctx(t), `
		SELECT passenger_id FROM ride_requests
		WHERE driver_id = $1 AND match_kind = 'micro_detour' AND status = 'offered'
	`, driverID).Scan(&offeredTo))
	assert.Equal(t, p1, offeredTo, "closest-ETA candidate wins the budget")
}

// The comparison baseline must be current-position → destination, not the
// origin-anchored route from SetRoute time.
func TestMicroDetour_BaselineFromCurrentPosition(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	mock := &testutil.MockOSRM{}
	testutil.SwapRouter(t, mock)
	enableMicroDetour(t, pool, driverID)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code)

	// Passenger appears near-miss AHEAD of the driver's mid-route position.
	dest := testutil.LatLng{Lat: 50.0, Lng: 20.1}
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0064, Lng: 20.07}, &dest)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	// Driver has driven half the route; the movement re-run triggers the
	// corridor + detour passes from (50.0, 20.05).
	w = testutil.PutJSON(t, r, "/driver/location", driverTok, map[string]any{
		"lat": 50.0, "lng": 20.05,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, payload := testutil.ReadEvent(t, paxConn, 3*time.Second)
	require.Equal(t, "ride_request", evtType)
	require.Equal(t, "micro_detour", payload["match_kind"])

	// The LAST 2-point route call is the pass baseline: it must start at
	// the driver's current position, not the route origin.
	calls := mock.RouteCallsSnapshot()
	require.NotEmpty(t, calls)
	baseline := calls[len(calls)-1]
	assert.InDelta(t, 50.0, baseline[0], 0.0005)
	assert.InDelta(t, 20.05, baseline[1], 0.0005, "baseline origin = CURRENT position")
	assert.InDelta(t, 50.0, baseline[2], 0.0005)
	assert.InDelta(t, 20.1, baseline[3], 0.0005)
}
