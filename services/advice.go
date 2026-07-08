package services

import (
	"context"
	"errors"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/jackc/pgx/v5"
)

// MatchAdvice diagnoses why a passenger may not be matching and carries concrete
// numbers the client turns into localized tips. Suggestions is an ordered list
// of stable codes (most useful first) that the app maps to copy.
type MatchAdvice struct {
	HasDestination bool     `json:"has_destination"`
	TripDistanceKm float64  `json:"trip_distance_km"`
	TripTooShort   bool     `json:"trip_too_short"`
	NearestRouteM  *float64 `json:"nearest_route_m"` // nil when no drivers are around
	DriversNearby  int      `json:"drivers_nearby"`
	DriversSameWay int      `json:"drivers_same_way"`
	LongWalkOn     bool     `json:"long_walk_on"`
	Suggestions    []string `json:"suggestions"`
}

// Suggestion codes (kept in sync with the Flutter advice card).
const (
	adviceNoLocation       = "no_location"
	adviceSetDestination   = "set_destination"
	adviceTripTooShort     = "trip_too_short"
	adviceWalkCloser       = "walk_closer"
	adviceEnableLongWalk   = "enable_long_walk"
	adviceNoSameWayDrivers = "no_same_way_drivers"
	adviceNoDrivers        = "no_drivers"
)

// GetMatchAdvice computes advice for the passenger from their stored location +
// destination against the live looking-driver pool. Read-only.
func GetMatchAdvice(passengerID string) (MatchAdvice, error) {
	ctx := context.Background()
	var a MatchAdvice

	var tripM float64
	var nearestM *float64
	err := db.DB.QueryRow(ctx, `
		WITH paxd AS (
			SELECT location AS loc,
			       COALESCE(allow_long_walk, false) AS long_walk,
			       CASE WHEN destination_lat IS NOT NULL
			            THEN ST_SetSRID(ST_MakePoint(destination_lng, destination_lat), 4326)
			            END AS dest
			FROM active_passengers WHERE passenger_id = $1
		),
		drv AS (
			SELECT ad.route AS route, ad.route::geometry AS rg
			FROM active_drivers ad, paxd
			WHERE ad.status = 'looking'
			  AND ad.created_at > NOW() - INTERVAL '30 minutes'
			  AND ad.driver_id <> $1
			  AND ad.route IS NOT NULL
			  AND ST_DWithin(ad.origin::geography, paxd.loc, 10000)
		)
		SELECT
			(SELECT long_walk FROM paxd),
			(SELECT dest IS NOT NULL FROM paxd),
			COALESCE((SELECT ST_Distance(loc, dest::geography) FROM paxd WHERE dest IS NOT NULL), 0),
			(SELECT COUNT(*) FROM drv),
			(SELECT MIN(ST_Distance((SELECT loc FROM paxd), route)) FROM drv),
			(SELECT COUNT(*) FROM drv, paxd
			   WHERE paxd.dest IS NOT NULL
			     AND ST_Distance(paxd.loc, drv.route) < $2
			     AND ST_Distance(paxd.dest::geography, drv.route) < $2
			     AND ST_LineLocatePoint(drv.rg, paxd.dest)
			         > ST_LineLocatePoint(drv.rg, paxd.loc::geometry))
	`, passengerID, config.C.MatchWalkLongM).Scan(
		&a.LongWalkOn, &a.HasDestination, &tripM,
		&a.DriversNearby, &nearestM, &a.DriversSameWay,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// No active_passengers row → location not shared yet.
		a.Suggestions = []string{adviceNoLocation}
		return a, nil
	}
	if err != nil {
		return a, err
	}

	a.TripDistanceKm = tripM / 1000
	a.TripTooShort = a.HasDestination && tripM > 0 && tripM < config.C.MatchShortTripM
	a.NearestRouteM = nearestM

	a.Suggestions = buildAdviceSuggestions(a)
	return a, nil
}

// buildAdviceSuggestions orders the advice codes most-useful-first.
func buildAdviceSuggestions(a MatchAdvice) []string {
	var out []string
	if !a.HasDestination {
		out = append(out, adviceSetDestination)
		return out
	}
	if a.TripTooShort {
		out = append(out, adviceTripTooShort)
	}
	effectiveCap := config.C.MatchWalkCapM
	if a.LongWalkOn {
		effectiveCap = config.C.MatchWalkLongM
	}
	if a.NearestRouteM != nil {
		n := *a.NearestRouteM
		// A driver passes just beyond your reach — walking toward the road helps.
		if n > effectiveCap && n <= config.C.MatchReachableWalkM {
			out = append(out, adviceWalkCloser)
		}
		// Turning on "longer walk" would bring an off-route driver in range.
		if !a.LongWalkOn && n > config.C.MatchWalkCapM && n <= config.C.MatchWalkLongM {
			out = append(out, adviceEnableLongWalk)
		}
	}
	if a.DriversSameWay == 0 {
		if a.DriversNearby == 0 {
			out = append(out, adviceNoDrivers)
		} else {
			out = append(out, adviceNoSameWayDrivers)
		}
	}
	return out
}
