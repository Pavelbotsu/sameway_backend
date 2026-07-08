package services

import (
	"context"
	"errors"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/jackc/pgx/v5"
)

// walkSpeedMps is the assumed pedestrian speed (~5 km/h) for converting a
// walk distance to a walk ETA in the reposition search.
const walkSpeedMps = 1.4

// RepositionResult is a suggested forward pickup: instead of the perpendicular
// projection, ask the passenger to walk to a point further along the route so
// they and the driver arrive together. All distances metres, times seconds.
type RepositionResult struct {
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	WalkM     float64 `json:"walk_m"`
	WalkEtaS  float64 `json:"walk_eta_s"`
	DriveEtaS float64 `json:"drive_eta_s"`
	CarryM    float64 `json:"carry_m"`
	// Worth is the deterministic go/no-go: the carry clears the coupling guard
	// and the passenger can reach P* no later than the driver. The UI should
	// only suggest the walk when Worth is true.
	Worth bool `json:"worth"`
}

// RepositionPickup finds the point P* on the driver's route, forward of the
// driver, that best synchronizes arrival — minimizing |drive_eta − walk_eta| —
// subject to: walk ≤ MatchWalkLongM, P* ahead of the driver, P* no further than
// the passenger's dropoff, and P* on a stoppable (non-fast) road. Pure geometry
// + arithmetic; **no LLM**. ok=false when the driver has no fresh route or no
// candidate qualifies.
//
// The driver never leaves their route — this only moves the *passenger* to a
// better point on the existing route, preserving the no-detour invariant.
func RepositionPickup(driverID, passengerID string) (RepositionResult, bool, error) {
	var r RepositionResult
	err := db.DB.QueryRow(context.Background(), `
		WITH ad AS (
			SELECT route::geometry AS rg, route_m AS rm, ST_Length(route) AS len,
			       ST_LineLocatePoint(route::geometry, current_location::geometry) AS dfrac,
			       ST_InterpolatePoint(route_m, current_location::geometry) AS dt
			FROM active_drivers
			WHERE driver_id = $1 AND route IS NOT NULL AND route_m IS NOT NULL
			  AND current_location IS NOT NULL
			  AND current_location_at > NOW() - make_interval(secs => $3::float8)
		),
		pax AS (
			SELECT location AS loc, destination_lat AS dlat, destination_lng AS dlng
			FROM active_passengers WHERE passenger_id = $2
		),
		params AS (
			SELECT CASE WHEN pax.dlat IS NOT NULL
			            THEN ST_LineLocatePoint(ad.rg,
			                   ST_SetSRID(ST_MakePoint(pax.dlng, pax.dlat), 4326))
			            ELSE 1.0 END AS dropoff_frac
			FROM ad, pax
		)
		SELECT m.lat, m.lng, m.walk_m, m.walk_s, m.drive_s, m.carry_m
		FROM ad, pax, params,
		     generate_series(0, 1, 0.01) AS f,
		     LATERAL (
		         SELECT ST_LineInterpolatePoint(ad.rg, f)                       AS pt,
		                ST_LineInterpolatePoint(ad.rg, GREATEST(f - 0.005, 0))  AS p1,
		                ST_LineInterpolatePoint(ad.rg, LEAST(f + 0.005, 1))     AS p2
		     ) g,
		     LATERAL (
		         SELECT ST_Y(g.pt) AS lat, ST_X(g.pt) AS lng,
		                ST_Distance(pax.loc, g.pt::geography)               AS walk_m,
		                ST_Distance(pax.loc, g.pt::geography) / $4::float8   AS walk_s,
		                ST_InterpolatePoint(ad.rm, g.pt) - ad.dt            AS drive_s,
		                SIGN(params.dropoff_frac - f) * ST_Length(ST_LineSubstring(
		                    ad.rg, LEAST(f, params.dropoff_frac), GREATEST(f, params.dropoff_frac)
		                )::geography)                                       AS carry_m,
		                CASE WHEN (ST_InterpolatePoint(ad.rm, g.p2) - ST_InterpolatePoint(ad.rm, g.p1)) > 0
		                     THEN ST_Distance(g.p1::geography, g.p2::geography)
		                          / (ST_InterpolatePoint(ad.rm, g.p2) - ST_InterpolatePoint(ad.rm, g.p1)) * 3.6
		                     ELSE 0 END                                     AS speed_kmh
		     ) m
		WHERE f > ad.dfrac
		  AND f <= params.dropoff_frac
		  AND m.walk_m <= $5::float8
		  AND m.drive_s >= 0
		  AND m.speed_kmh < $6::float8
		ORDER BY abs(m.drive_s - m.walk_s) ASC
		LIMIT 1
	`,
		driverID, passengerID,
		config.C.MatchLocFreshS,   // $3
		walkSpeedMps,              // $4
		config.C.MatchWalkLongM,   // $5
		config.C.MatchFastRoadKmh, // $6
	).Scan(&r.Lat, &r.Lng, &r.WalkM, &r.WalkEtaS, &r.DriveEtaS, &r.CarryM)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepositionResult{}, false, nil
	}
	if err != nil {
		return RepositionResult{}, false, err
	}
	// Deterministic worth gate: the carry justifies the walk and the passenger
	// arrives no later than the driver.
	r.Worth = r.CarryM >= config.C.MatchCarryK*r.WalkM && r.WalkEtaS <= r.DriveEtaS
	return r, true, nil
}
