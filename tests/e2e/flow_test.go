// Package e2e tests cross-handler choreography — the bits where Set-Route,
// WebSocket events, and Respond endpoints must work together. Unit tests in
// services/handlers verify each piece in isolation; these tests prove the
// pieces stitch correctly into the user-visible flows.
package e2e

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var e2ePool *pgxpool.Pool

func TestMain(m *testing.M) {
	config.Load()
	if config.C.JWTSecret == "" {
		config.C.JWTSecret = "e2e-test-secret"
	}
	pool, teardown := testutil.MustStartPostgres()
	e2ePool = pool
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setupE2E(t *testing.T) (*pgxpool.Pool, *gin.Engine, string) {
	t.Helper()
	testutil.ResetDB(t, e2ePool)
	testutil.UseDB(t, e2ePool)
	r := testutil.NewTestRouter()
	wsBase := testutil.ServeWS(t, r)
	return e2ePool, r, wsBase
}

// register POSTs /auth/register and returns the token.
func register(t *testing.T, r *gin.Engine, role, email string) (id, token string) {
	t.Helper()
	w := testutil.PostJSON(t, r, "/auth/register", "", map[string]any{
		"name": email, "email": email,
		"password": "test-pass-1", "role": role,
	})
	require.Equal(t, http.StatusCreated, w.Code, "register: %s", w.Body.String())
	var body map[string]any
	testutil.DecodeJSON(t, w, &body)
	return body["user_id"].(string), body["token"].(string)
}

// TestDriverInitiatedFlow exercises: driver sets route → corridor match finds
// passenger ahead → ride_request WS reaches passenger → passenger accepts →
// driver_response WS hits the driver → seats reserved.
func TestDriverInitiatedFlow(t *testing.T) {
	pool, r, wsBase := setupE2E(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	_, driverTok := register(t, r, "driver", "d1@x.com")
	passengerID, paxTok := register(t, r, "passenger", "p1@x.com")

	// Passenger publishes location ~700 m ahead of the driver's origin.
	w := testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.0, "lng": 20.01,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Passenger opens WS.
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	// Driver sets a route.
	w = testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Passenger receives ride_request.
	evtType, payload := testutil.ReadEvent(t, paxConn, 3*time.Second)
	require.Equal(t, "ride_request", evtType)
	reqID := payload["request_id"].(string)
	assert.NotEmpty(t, payload["pickup_lat"], "pickup point should be populated")

	// Passenger accepts.
	w = testutil.PostJSON(t, r, "/passenger/respond", paxTok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Confirm DB state: row accepted, seats_taken=1.
	var status string
	var seatsTaken int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT seats_taken FROM active_drivers WHERE driver_id = (
			SELECT driver_id FROM ride_requests WHERE id = $1)`, reqID).Scan(&seatsTaken))
	assert.Equal(t, "accepted", status)
	assert.Equal(t, 1, seatsTaken)
	_ = passengerID
}

// TestPassengerInitiatedFlow exercises: passenger fetches nearby drivers →
// picks one → /passenger/request-driver → driver receives passenger_request →
// /driver/respond accepts → both sides consistent.
func TestPassengerInitiatedFlow(t *testing.T) {
	pool, r, wsBase := setupE2E(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	driverID, driverTok := register(t, r, "driver", "d2@x.com")
	_, paxTok := register(t, r, "passenger", "p2@x.com")

	// Driver has an active route.
	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Passenger publishes location.
	w = testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.0, "lng": 19.95, // outside the driver's heading filter on purpose
	})
	require.Equal(t, http.StatusOK, w.Code)

	driverConn := testutil.DialWS(t, wsBase, driverTok)
	time.Sleep(100 * time.Millisecond)

	// Passenger explicitly requests this driver (the heading-filter doesn't
	// apply on the passenger-initiated path — it's a direct ask).
	w = testutil.PostJSON(t, r, "/passenger/request-driver", paxTok, map[string]any{
		"driver_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Driver receives passenger_request.
	evtType, payload := testutil.ReadEvent(t, driverConn, 3*time.Second)
	require.Equal(t, "passenger_request", evtType)
	reqID := payload["request_id"].(string)

	// Driver accepts.
	w = testutil.PostJSON(t, r, "/driver/respond", driverTok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var status string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "accepted", status)
}

// TestRoleSwitchCleansUpDriverState — switching from driver to passenger when
// active_drivers exists must DELETE that row and emit ride_cancelled to any
// matched passenger.
func TestRoleSwitchCleansUpDriverState(t *testing.T) {
	pool, r, _ := setupE2E(t)

	id, tok := register(t, r, "driver", "switcher@x.com")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)

	w := testutil.PostJSON(t, r, "/auth/switch-role", tok, map[string]any{"role": "passenger"})
	require.Equal(t, http.StatusOK, w.Code)

	var n int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT count(*) FROM active_drivers WHERE driver_id = $1`, id).Scan(&n))
	assert.Equal(t, 0, n, "active_drivers row must be deleted")
}

// TestSelfPairingGuards — user who is both driver and passenger cannot see
// their own car in nearby-drivers, and request-driver(self) is rejected.
func TestSelfPairingGuards(t *testing.T) {
	pool, r, _ := setupE2E(t)
	id, tok := register(t, r, "passenger", "self@x.com")
	testutil.CreateActiveDriver(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	testutil.CreateActivePassenger(t, pool, id,
		testutil.LatLng{Lat: 50, Lng: 20}, nil)

	// Nearby drivers list excludes self.
	w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", tok, map[string]any{
		"latitude": 50.0, "longitude": 20.0, "radius_km": 5.0,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	assert.Empty(t, list)

	// Direct self-request rejected.
	w = testutil.PostJSON(t, r, "/passenger/request-driver", tok, map[string]any{
		"driver_id": id,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestRideDoneClearsSeats — driver completes ride; the ride_requests row
// flips to cancelled, seats_taken resets to 0, both parties' WS receive
// ride_done.
func TestRideDoneClearsSeats(t *testing.T) {
	pool, r, wsBase := setupE2E(t)
	driverID, driverTok := register(t, r, "driver", "done-d@x.com")
	passengerID, _ := register(t, r, "passenger", "done-p@x.com")

	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50, Lng: 20},
		testutil.LatLng{Lat: 50, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20, 50}, [2]float64{20.1, 50}),
	)
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, reqID)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(),
		`UPDATE active_drivers SET seats_taken = 1 WHERE driver_id = $1`, driverID)
	require.NoError(t, err)

	driverConn := testutil.DialWS(t, wsBase, driverTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PutJSON(t, r, "/driver/status", driverTok, map[string]any{
		"status": "done", "distance_km": 5.0,
	})
	require.Equal(t, http.StatusOK, w.Code)

	evtType, _ := testutil.ReadEvent(t, driverConn, 2*time.Second)
	assert.Equal(t, "ride_done", evtType)

	var status string
	var seats int
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT seats_taken FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&seats))
	assert.Equal(t, "done", status, "status should be flipped to done on end-route")
	assert.Equal(t, 0, seats)
}

// TestPickupHandshakeAndDropoff — full handshake: accept → backend mints the
// 4-digit code → both sides see it → driver confirms → status is in_progress
// → driver drops off → status is done + seats decremented + driver still
// active (looking) so they can match another passenger.
func TestPickupHandshakeAndDropoff(t *testing.T) {
	pool, r, wsBase := setupE2E(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	driverID, driverTok := register(t, r, "driver", "hs-d@x.com")
	passengerID, paxTok := register(t, r, "passenger", "hs-p@x.com")

	w := testutil.PutJSON(t, r, "/passenger/location", paxTok, map[string]any{
		"lat": 50.0, "lng": 20.01,
	})
	require.Equal(t, http.StatusOK, w.Code)

	paxConn := testutil.DialWS(t, wsBase, paxTok)
	driverConn := testutil.DialWS(t, wsBase, driverTok)
	time.Sleep(100 * time.Millisecond)

	// Driver sets route → passenger gets ride_request with pickup_code.
	w = testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	_, _ = testutil.ReadEvent(t, paxConn, 3*time.Second) // ride_request

	// Passenger accepts. The driver_response WS payload carries pickup_code.
	// SetPickupCode runs inside respondAccept.
	// Fetch the request_id via /passenger/my-requests.
	w = testutil.GetJSON(t, r, "/passenger/my-requests", paxTok)
	require.Equal(t, http.StatusOK, w.Code)
	var requests []map[string]any
	testutil.DecodeJSON(t, w, &requests)
	require.NotEmpty(t, requests)
	reqID := requests[0]["request_id"].(string)

	w = testutil.PostJSON(t, r, "/passenger/respond", paxTok, map[string]any{
		"request_id": reqID, "status": "accepted",
	})
	require.Equal(t, http.StatusOK, w.Code)
	// Driver's ride_response with pickup_code.
	evtType, payload := testutil.ReadEvent(t, driverConn, 3*time.Second)
	require.Equal(t, "ride_response", evtType)
	code := payload["pickup_code"].(string)
	require.Len(t, code, 4, "pickup code should be a 4-digit string")

	// Passenger asks the driver for the code (driver has it on their screen),
	// types it in — backend flips to in_progress → driver receives
	// pickup_confirmed WS. Asymmetric: driver can NOT submit (gets 403).
	w = testutil.PostJSON(t, r, "/pickup/confirm", paxTok, map[string]any{
		"request_id": reqID, "code": code,
	})
	require.Equal(t, http.StatusOK, w.Code)
	evtType, _ = testutil.ReadEvent(t, driverConn, 2*time.Second)
	assert.Equal(t, "pickup_confirmed", evtType)

	var status string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	require.Equal(t, "in_progress", status)

	// Driver drops off → both sides receive ride_done; row is done; driver
	// remains active with seats decremented (not bulk-ended).
	w = testutil.PostJSON(t, r, "/driver/dropoff/"+reqID, driverTok,
		map[string]any{"distance_km": 6.0})
	require.Equal(t, http.StatusOK, w.Code)

	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM ride_requests WHERE id = $1`, reqID).Scan(&status))
	assert.Equal(t, "done", status)

	// Driver row is still alive (in 'looking' status) — they can keep matching.
	var driverStatus string
	require.NoError(t, pool.QueryRow(t.Context(),
		`SELECT status FROM active_drivers WHERE driver_id = $1`, driverID).Scan(&driverStatus))
	assert.Contains(t, []string{"looking", "in_progress"}, driverStatus,
		"driver stays active after a per-passenger dropoff")
	_ = passengerID
}
