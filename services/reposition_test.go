package services_test

import (
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A passenger on the route with a forward destination gets a forward pickup
// suggestion that synchronizes arrival (walk_eta ≤ drive_eta) and is worth it.
func TestRepositionPickup_SuggestsForwardPoint(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	_, _ = seedLongRouteDriver(t, pool, driverID) // driver at origin, ~30 km/h

	// Passenger on the route ~3.6 km along, heading to ~10.7 km along.
	paxDest := testutil.LatLng{Lat: 50.00, Lng: 20.15}
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.05}, &paxDest)

	res, ok, err := services.RepositionPickup(driverID, passengerID)
	require.NoError(t, err)
	require.True(t, ok, "a reachable forward point should exist")
	assert.Greater(t, res.Lng, 20.05, "P* should be forward of the passenger's projection")
	assert.Less(t, res.WalkM, 2000.0, "within the long-walk cap")
	assert.LessOrEqual(t, res.WalkEtaS, res.DriveEtaS, "passenger arrives no later than the driver")
	assert.True(t, res.Worth, "good carry + synced arrival → worth suggesting")
}

// No active driver (or no route) → no suggestion, no error.
func TestRepositionPickup_NoDriver(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool,
		passengerID, testutil.LatLng{Lat: 50.00, Lng: 20.05}, nil)

	_, ok, err := services.RepositionPickup(driverID, passengerID)
	require.NoError(t, err)
	assert.False(t, ok, "no active route → no suggestion")
}
