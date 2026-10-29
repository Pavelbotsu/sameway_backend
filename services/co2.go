package services

import (
	"context"

	"github.com/Pavelbotsu/sameway_backend/db"
)

const co2PerKm = 0.12 // kg CO2 saved per km shared (avg car ~120 g/km)

// maxClientReportedKm caps a self-reported ride length when the server cannot
// measure it. A shared city ride is single-digit to low-double-digit km; the
// old 2000 km ceiling let one request mint several years' worth of "CO2 saved".
const maxClientReportedKm = 200

// CarriedDistanceKm measures how far the passenger was ACTUALLY carried, from
// the ride's stored pickup projection to the driver's current position along
// the driver's own route.
//
// Distance previously came from the request body and fed the CO2 headline stat
// and the trip leaderboard directly, bounded only by 0 < d <= 2000 — so a
// client could claim 2000 km per ride, and the "end driving day" path applied
// one client-supplied number to every passenger in the car at once. Everything
// needed to compute it honestly is already stored.
//
// Returns ok=false when it can't be derived (route or pickup missing, driver
// gone offline before the dropoff was recorded); callers fall back to the
// client value, clamped.
func CarriedDistanceKm(ctx context.Context, rideRequestID string) (float64, bool) {
	var km float64
	err := db.DB.QueryRow(ctx, `
		SELECT ST_Length(ST_LineSubstring(
		           ad.route::geometry,
		           LEAST(f.pickup_frac, f.now_frac),
		           GREATEST(f.pickup_frac, f.now_frac)
		       )::geography) / 1000.0
		FROM ride_requests rr
		JOIN active_drivers ad ON ad.driver_id = rr.driver_id
		CROSS JOIN LATERAL (
		    SELECT ST_LineLocatePoint(ad.route::geometry, rr.pickup_location::geometry) AS pickup_frac,
		           ST_LineLocatePoint(ad.route::geometry,
		               COALESCE(ad.current_location, ad.destination)::geometry)         AS now_frac
		) f
		WHERE rr.id = $1
		  AND ad.route IS NOT NULL
		  AND rr.pickup_location IS NOT NULL
	`, rideRequestID).Scan(&km)
	if err != nil || km <= 0 {
		return 0, false
	}
	return km, true
}

// RecordCompletedTrip stores one completed-trip row per ride request —
// uniq_completed_trips_request is the ON CONFLICT arbiter, so retried
// dropoffs / repeated "done" calls can't double-count km or CO2. Distance is
// defensively bounded here as well as at the handlers (it is client-reported
// until server-side route measurement lands).
func RecordCompletedTrip(rideRequestID, driverID, passengerID string, distanceKm float64) error {
	// Prefer the server-measured distance over whatever the client reported.
	// The client value is only a fallback for rides we can't reconstruct (no
	// route on file), and is clamped hard so it can't inflate the stats.
	if measured, ok := CarriedDistanceKm(context.Background(), rideRequestID); ok {
		distanceKm = measured
	} else if distanceKm > maxClientReportedKm {
		distanceKm = maxClientReportedKm
	}
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
