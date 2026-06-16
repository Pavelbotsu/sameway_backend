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

// ── FindPassengersInCorridor ─────────────────────────────────────────────────

func TestFindPassengersInCorridor_ExcludesPassengerBehind(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	// Driver heading east from (50.00, 20.00) to (50.00, 20.10).
	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Passenger BEHIND the driver (slightly west of origin).
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 19.99}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "passenger behind driver must not match")
}

func TestFindPassengersInCorridor_IncludesPassengerAhead(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Passenger ~700 m AHEAD on the road (lng 20.01 ≈ 700 m east at lat 50).
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.01}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	m := matches[0]
	assert.Equal(t, passengerID, m.User.ID)
	assert.Less(t, m.WalkM, 100.0, "on-road passenger should walk <100 m")
	assert.InDelta(t, 700.0, m.DriverToPickupM, 100.0, "driver should be ~700 m from pickup")
}

func TestFindPassengersInCorridor_DropsBeyondEarlyNotify(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	// Long route. Passenger is on it but ~5 km ahead — beyond the 2 km cap.
	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.20}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.20, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.07}, nil) // ~5 km along

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "passenger beyond earlyNotifyRadiusM must be skipped")
}

func TestFindPassengersInCorridor_DestinationFilter(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")

	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Passenger ahead on the route — but their declared destination is far
	// from the driver's destination → excluded by the 1.5 km destination
	// match cap.
	farDest := testutil.LatLng{Lat: 51.00, Lng: 20.10}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.01}, &farDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches, "destination mismatch must exclude")

	// Swap to a destination near the driver's → now matches.
	nearDest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.01}, &nearDest)

	matches, err = services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

// TestFindPassengersInCorridor_DestinationAnchored_OutsideCorridor verifies
// the relaxed branch of the disjunctive WHERE: a passenger far outside the
// driver's corridor but heading to the same destination must still match.
//
// Geometry: driver route along the equator from 20.00→20.10 (~7.1 km E-W).
// Passenger sits ~4.4 km north of the route at 50.04. With a 1 km corridor
// the strict path drops them. Since their destination equals the driver's,
// the relaxed path (walk < 5 km, ahead < 10 km) admits them and the match
// is tagged "destination_anchored".
func TestFindPassengersInCorridor_DestinationAnchored_OutsideCorridor(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d-da-1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p-da-1")

	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Passenger ~4.4 km north of the route (50.04 vs 50.00 ≈ 4.4 km), with
	// destination matching the driver's. Strict corridor (1 km) drops them;
	// relaxed branch should match.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.04, Lng: 20.05}, &paxDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1, "destination-anchored relaxed match must fire")
	assert.Equal(t, "destination_anchored", matches[0].MatchKind)
	assert.Greater(t, matches[0].WalkM, 4000.0,
		"walk distance should reflect the actual 4+ km offset")
}

// TestFindPassengersInCorridor_MatchKindCorridor verifies the strict branch
// still labels its matches as "corridor".
func TestFindPassengersInCorridor_MatchKindCorridor(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d-mc-1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p-mc-1")

	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Passenger ahead on the route, ~111 m from the route center line and
	// ~700 m along the route from the driver — inside the 1 km corridor,
	// 1 km walk cap, AND the 2 km early-notify ahead cap. No destination.
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.001, Lng: 20.01}, nil)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, "corridor", matches[0].MatchKind)
}

// TestFindPassengersInCorridor_RelaxedRequiresDestMatch verifies a passenger
// outside the corridor with NO destination match is still dropped — relaxed
// path is destination-gated.
func TestFindPassengersInCorridor_RelaxedRequiresDestMatch(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d-rrdm-1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p-rrdm-1")

	origin := testutil.LatLng{Lat: 50.00, Lng: 20.00}
	dest := testutil.LatLng{Lat: 50.00, Lng: 20.10}
	routeWKT := testutil.LineStringWKT(
		[2]float64{20.00, 50.00},
		[2]float64{20.10, 50.00},
	)
	testutil.CreateActiveDriver(t, pool, driverID, origin, dest, routeWKT)

	// Same far-away passenger but with a completely different destination.
	// Strict path drops them (corridor too narrow). Relaxed path drops them
	// (destinations don't match). Result: 0 matches.
	farDest := testutil.LatLng{Lat: 60.00, Lng: 30.00}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.04, Lng: 20.05}, &farDest)

	matches, err := services.FindPassengersInCorridor(driverID, routeWKT, 1.0, dest.Lat, dest.Lng, 10)
	require.NoError(t, err)
	assert.Empty(t, matches,
		"relaxed path must require destination match — out-of-corridor + wrong dest stays excluded")
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

	require.NoError(t, services.UpsertActiveDriver(driverID,
		testutil.LineStringWKT([2]float64{20.00, 50.00}, [2]float64{20.10, 50.00}),
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
