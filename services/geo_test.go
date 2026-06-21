package services_test

import (
	"context"
	"os"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// servicesPool is the shared pool for this test file. One container start
// amortizes across all the service tests in this package.
var servicesPool *pgxpool.Pool

func TestMain(m *testing.M) {
	pool, teardown := testutil.MustStartPostgres()
	servicesPool = pool
	code := m.Run()
	teardown()
	os.Exit(code)
}

// setup truncates the DB and points production code at the test pool.
func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testutil.ResetDB(t, servicesPool)
	testutil.UseDB(t, servicesPool)
	return servicesPool
}

// ── CreateRideRequest ────────────────────────────────────────────────────────

func TestCreateRideRequest_NewRow(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id, isNew, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)
	require.True(t, isNew, "first call should create a fresh row")
	require.NotEmpty(t, id)
}

func TestCreateRideRequest_IdempotentOnExistingPending(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id1, isNew1, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)
	require.True(t, isNew1)

	id2, isNew2, err := services.CreateRideRequest(driverID, passengerID, 51.0, 21.0)
	require.NoError(t, err)
	require.False(t, isNew2, "second call should NOT report a new row")
	require.Equal(t, id1, id2, "same id returned")

	// And no mutation: pickup_location remains the original (0,0 → 50,20 lat lng).
	var status string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM ride_requests WHERE id = $1`, id1).Scan(&status))
	require.Equal(t, "pending", status)
}

func TestCreateRideRequest_IdempotentOnAcceptedRow(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id1, _, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)

	// Manually flip to accepted (simulating /driver/respond).
	_, err = pool.Exec(context.Background(),
		`UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, id1)
	require.NoError(t, err)

	id2, isNew, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)
	require.False(t, isNew, "no clobbering an accepted row")
	require.Equal(t, id1, id2)

	var status string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM ride_requests WHERE id = $1`, id1).Scan(&status))
	require.Equal(t, "accepted", status, "status must NOT be reset to pending")
}

func TestCreateRideRequest_RevivesAfterCancellation(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id1, _, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)

	_, err = pool.Exec(context.Background(),
		`UPDATE ride_requests SET status = 'cancelled' WHERE id = $1`, id1)
	require.NoError(t, err)

	id2, isNew, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)
	require.True(t, isNew, "cancelled history shouldn't block a fresh row")
	require.NotEqual(t, id1, id2, "new row, distinct id")

	// Confirm both rows now exist.
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ride_requests
		 WHERE driver_id = $1 AND passenger_id = $2`,
		driverID, passengerID).Scan(&n))
	require.Equal(t, 2, n)
}

// ── FindPassengersInCorridor (ETA-window + same-way model) ───────────────────
//
// Shared geometry: a straight E-W driver route at lat 50.00 from lng 20.00 to
// 20.20 (~14.3 km; at the seed's constant 30 km/h that's ~1721 s total). The
// driver sits at the origin (frac 0). With the default tunables the ETA window
// (90–600 s) admits pickups projecting between ~lng 20.0105 (~750 m) and
// ~lng 20.0697 (~5000 m) ahead; min-ahead is 300 m; default walk cap 600 m;
// min carry 2 km. Helper returns the route WKT + driver destination.
func seedLongRouteDriver(t *testing.T, pool *pgxpool.Pool, driverID string) (dest testutil.LatLng, routeWKT string) {
	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest = testutil.LatLng{Lat: 50.00, Lng: 20.20}
	routeWKT = testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.20, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)
	return
}

func TestFindPassengersInCorridor_ExcludesPassengerBehind(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	// Passenger BEHIND the driver (west of origin).
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 19.99}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "passenger behind driver must not match")
}

// Ideal case (#1): passenger on-route, ahead, inside the ETA window, with a
// destination that projects forward → "corridor" with a carry well over 2 km.
func TestFindPassengersInCorridor_OnRouteForwardWithDest(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	// Pickup ~lng 20.03 (~2.1 km along, ~258 s — inside window), ~55 m off the
	// line; dropoff at lng 20.10 (~7.2 km along) → carry ~5 km.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.03}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	m := matches[0]
	assert.Equal(t, passengerID, m.User.ID)
	assert.Equal(t, "corridor", m.MatchKind)
	assert.Less(t, m.WalkM, 100.0, "on-road passenger walks <100 m")
	assert.Greater(t, m.EtaPickupS, 90.0)
	assert.Less(t, m.EtaPickupS, 600.0)
	assert.Greater(t, m.CarryM, 2000.0, "carry must clear the 2 km minimum")
}

// No destination (#15): weak, pickup-only match tagged "corridor_weak".
func TestFindPassengersInCorridor_NoDestWeak(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.03}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "corridor_weak", matches[0].MatchKind)
}

// Below the ETA lower bound ("about to pass / just passed"): ~430 m / ~51 s.
func TestFindPassengersInCorridor_BelowEtaMin(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.006}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "pickup inside the ETA lower bound must be skipped")
}

// Beyond the ETA upper bound: ~6.5 km / ~774 s — defer until the driver nears.
func TestFindPassengersInCorridor_BeyondEtaMax(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.09}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "pickup beyond the ETA upper bound must be deferred")
}

// Driver has driven past the pickup (#11): move current_location downstream of
// the projected pickup → no match.
func TestFindPassengersInCorridor_DriverPassed(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.03}, nil)
	// Driver now well past the pickup at lng 20.08.
	testutil.SetDriverCurrentLocation(t, pool, driverID, testutil.LatLng{Lat: 50.00, Lng: 20.08})

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "driver past the pickup must not match")
}

// Short overlap (#6): carry below the 2 km minimum → rejected.
func TestFindPassengersInCorridor_ShortCarryRejected(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	// Pickup ~20.03, dropoff ~20.035 → carry ~360 m.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.035}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.03}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "carry under the minimum must be rejected")
}

// Dropoff behind pickup (#8): destination projects before the pickup → rejected.
func TestFindPassengersInCorridor_DropoffBehindPickup(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	// Pickup ~20.05, dropoff ~20.03 (earlier along the route).
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.03}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.05}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "backward dropoff must be rejected")
}

// Walk-in beyond the cap: passenger ~2.2 km off the route → rejected even
// though they're abeam an in-window point.
func TestFindPassengersInCorridor_WalkTooFar(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.02, Lng: 20.03}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "walk beyond the cap must be rejected")
}

// Stale driver location: no fresh fix → the driver is skipped entirely (no
// origin fallback that would make the whole route read as ahead).
func TestFindPassengersInCorridor_StaleLocationSkipped(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedLongRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.03}, nil)
	testutil.SetDriverLocationStale(t, pool, driverID, testutil.LatLng{Lat: 50.00, Lng: 20.00})

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "driver with a stale location must be skipped")
}

// seedFastRouteDriver builds a ~28.7 km E-W route whose measured line implies
// ~100 km/h everywhere (a "fast road"). At that speed the ETA window admits
// pickups projecting between ~lng 20.035 and ~20.232.
func seedFastRouteDriver(t *testing.T, pool *pgxpool.Pool, driverID string) (dest testutil.LatLng, routeWKT string) {
	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest = testutil.LatLng{Lat: 50.00, Lng: 20.40}
	routeWKT = testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.40, 50.00})
	// ~28.7 km at ~100 km/h ≈ 1033 s for the single segment.
	routeM := services.BuildMeasuredLineEWKT(
		[][2]float64{{20.00, 50.00}, {20.40, 50.00}}, []float64{1033})
	testutil.CreateActiveDriverMeasured(t, pool, driverID, origin, dest, routeWKT, routeM)
	return
}

// Road class (#8): on a fast road a medium carry that would pass in the city is
// rejected — it must clear the higher fast-road minimum.
func TestFindPassengersInCorridor_FastRoadRejectsMediumCarry(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedFastRouteDriver(t, pool, driverID)

	// Pickup ~3.6 km along (eta ~129 s), carry ~7 km: over the 2 km city min,
	// under the 15 km fast-road min → rejected on a fast road.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.15}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.05}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "medium carry on a fast road must be rejected")
}

// Road class (#8): a long carry on the same fast road clears the fast minimum
// and matches.
func TestFindPassengersInCorridor_FastRoadLongCarryMatches(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedFastRouteDriver(t, pool, driverID)

	// Pickup ~3.6 km along, dropoff at the route end → carry ~25 km ≥ 15 km.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.40}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.05}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1, "long carry on a fast road should match")
	assert.Equal(t, "corridor", matches[0].MatchKind)
}

// Road class (#8): a no-destination (weak) passenger on a fast road is dropped —
// you can't be flagged down on a motorway.
func TestFindPassengersInCorridor_FastRoadDropsWeak(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	dest, routeWKT := seedFastRouteDriver(t, pool, driverID)

	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.0005, Lng: 20.05}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "weak match on a fast road must be dropped")
}

// ── GetNearbyDrivers self-exclusion ─────────────────────────────────────────

func TestGetNearbyDrivers_ExcludesSelf(t *testing.T) {
	pool := setup(t)
	otherDriverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	selfID, _ := testutil.CreateUser(t, pool, "driver", "self") // self is also a driver

	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, otherDriverID,
		testutil.LatLng{Lat: 50, Lng: 20}, testutil.LatLng{Lat: 50, Lng: 20.10}, routeWKT)
	testutil.CreateActiveDriver(t, pool, selfID,
		testutil.LatLng{Lat: 50, Lng: 20}, testutil.LatLng{Lat: 50, Lng: 20.10}, routeWKT)

	drivers, err := services.GetNearbyDrivers(50.0, 20.0, 5.0, 0, 0, 0, selfID)
	require.NoError(t, err)
	require.Len(t, drivers, 1, "self must be excluded")
	assert.Equal(t, otherDriverID, drivers[0].DriverID)
}

// ── GetDriverRequests filters + joins ───────────────────────────────────────

func TestGetDriverRequests_FiltersStatus(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	id, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)
	// Insert a separate cancelled row directly (the partial unique index
	// permits multiple non-active rows for the same pair).
	_, err = pool.Exec(context.Background(),
		`INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status)
		 VALUES ($1, $2, ST_MakePoint(20, 50)::geography, 'cancelled')`,
		driverID, passengerID)
	require.NoError(t, err)

	reqs, err := services.GetDriverRequests(driverID)
	require.NoError(t, err)
	require.Len(t, reqs, 1, "only pending/accepted must be returned")
	assert.Equal(t, id, reqs[0].ID)
	assert.Equal(t, "pending", reqs[0].Status)
}

func TestGetDriverRequests_JoinsName(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	_, _ = testutil.CreateUser(t, pool, "passenger", "Alice")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "Bob")

	_, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	reqs, err := services.GetDriverRequests(driverID)
	require.NoError(t, err)
	require.Len(t, reqs, 1)
	assert.Equal(t, "Bob", reqs[0].PassengerName)
}

// ── UpsertActiveDriver seeds current_location ───────────────────────────────

func TestUpsertActiveDriver_SeedsCurrentLocation(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")

	routeWKT := testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.10, 50.00})
	require.NoError(t, services.UpsertActiveDriver(driverID,
		routeWKT,
		services.BuildMeasuredLineEWKT([][2]float64{{20.00, 50.00}, {20.10, 50.00}}, nil),
		50.0, 20.0, 50.0, 20.10, 1.0, 3,
	))

	var locLat, locLng, originLat, originLng float64
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT ST_Y(current_location::geometry), ST_X(current_location::geometry),
		       ST_Y(origin::geometry),           ST_X(origin::geometry)
		FROM active_drivers WHERE driver_id = $1
	`, driverID).Scan(&locLat, &locLng, &originLat, &originLng))
	assert.Equal(t, originLat, locLat)
	assert.Equal(t, originLng, locLng)
}
