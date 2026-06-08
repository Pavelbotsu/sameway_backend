package services_test

import (
	"context"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordCompletedTrip_StoresCO2(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50.0, 20.0)
	require.NoError(t, err)

	require.NoError(t, services.RecordCompletedTrip(reqID, driverID, passengerID, 10.0))

	var co2, km float64
	var trips int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT co2_saved_kg, distance_km, 1 FROM completed_trips
		 WHERE ride_request_id = $1`, reqID).Scan(&co2, &km, &trips))
	// 10 km × 0.12 kg/km = 1.2 kg
	assert.InDelta(t, 1.2, co2, 0.0001)
	assert.Equal(t, 10.0, km)
}

func TestGetUserStats_AccumulatesAcrossTrips(t *testing.T) {
	pool := setup(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	pax1, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	pax2, _ := testutil.CreateUser(t, pool, "passenger", "p2")

	r1, _, err := services.CreateRideRequest(driverID, pax1, 0, 0)
	require.NoError(t, err)
	r2, _, err := services.CreateRideRequest(driverID, pax2, 0, 0)
	require.NoError(t, err)

	require.NoError(t, services.RecordCompletedTrip(r1, driverID, pax1, 5.0))
	require.NoError(t, services.RecordCompletedTrip(r2, driverID, pax2, 7.5))

	stats, err := services.GetUserStats(driverID)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.TripsCount)
	assert.InDelta(t, 12.5, stats.KmShared, 0.0001)
	assert.InDelta(t, 1.5, stats.CO2SavedKg, 0.0001) // 12.5 × 0.12
}

func TestGetUserStats_ZeroForUnknownUser(t *testing.T) {
	setup(t)
	stats, err := services.GetUserStats("00000000-0000-0000-0000-000000000000")
	require.NoError(t, err)
	assert.Equal(t, 0, stats.TripsCount)
	assert.Equal(t, 0.0, stats.KmShared)
	assert.Equal(t, 0.0, stats.CO2SavedKg)
}
