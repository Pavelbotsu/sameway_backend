package services

import (
	"context"

	"github.com/Pavelbotsu/sameway_backend/db"
)

const co2PerKm = 0.12 // kg CO2 saved per km shared (avg car ~120 g/km)

// RecordCompletedTrip stores one completed-trip row per ride request —
// uniq_completed_trips_request is the ON CONFLICT arbiter, so retried
// dropoffs / repeated "done" calls can't double-count km or CO2. Distance is
// defensively bounded here as well as at the handlers (it is client-reported
// until server-side route measurement lands).
func RecordCompletedTrip(rideRequestID, driverID, passengerID string, distanceKm float64) error {
	if distanceKm <= 0 || distanceKm > 2000 {
		return nil
	}
	co2 := distanceKm * co2PerKm
	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO completed_trips (ride_request_id, driver_id, passenger_id, distance_km, co2_saved_kg)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (ride_request_id) WHERE ride_request_id IS NOT NULL DO NOTHING
	`, rideRequestID, driverID, passengerID, distanceKm, co2)
	return err
}

type UserStats struct {
	CO2SavedKg float64 `json:"co2_saved_kg"`
	TripsCount int     `json:"trips_count"`
	KmShared   float64 `json:"km_shared"`
}

func GetUserStats(userID string) (UserStats, error) {
	var s UserStats
	err := db.DB.QueryRow(context.Background(), `
		SELECT
			COALESCE(SUM(co2_saved_kg), 0),
			COUNT(*),
			COALESCE(SUM(distance_km), 0)
		FROM completed_trips
		WHERE driver_id = $1 OR passenger_id = $1
	`, userID).Scan(&s.CO2SavedKg, &s.TripsCount, &s.KmShared)
	return s, err
}
