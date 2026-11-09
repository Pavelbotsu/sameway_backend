package services

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/jackc/pgx/v5"
)

// Matching thresholds now live in config.C (Match*), populated from env with
// defaults — see config/config.go. The former file-level consts
// (maxWalkM / earlyNotifyRadiusM / destinationMatchMeters / relaxed*) were
// replaced by the ETA-window + same-way rules in FindPassengersInCorridor.

// CorridorMatch is the result row of the same-way corridor query: the matched
// passenger, the pickup point projected onto the driver's road, and the metrics
// the UI + ranker need. MatchKind is "corridor" (passenger declared a
// destination that projects forward onto the route) or "corridor_weak" (no
// destination declared — pickup-only confidence).
type CorridorMatch struct {
	User            models.User
	PickupLat       float64
	PickupLng       float64
	WalkM           float64 // off-route walk to the pickup (walk-in)
	DriverToPickupM float64 // along-route distance driver → pickup (signed; >0 ahead)
	EtaPickupS      float64 // driver's travel time to the pickup, seconds
	CarryM          float64 // on-route distance carried (pickup → dropoff); 0 if no dest
	WalkOutM        float64 // off-route walk from dropoff to destination; 0 if no dest
	MatchKind       string  // "corridor" | "corridor_weak" | "partial_relay"
	// Partial relay (Round 2 §1): the dropoff is the route END the passenger
	// rides to before continuing on their own. TransitStop* describe the
	// nearest stop within GTFSStopRadiusM ("" / 0 when the feasibility came
	// from second-leg driver density instead).
	DropoffLat       float64
	DropoffLng       float64
	TransitStopName  string
	TransitStopDistM float64
	// Micro-detour (Round 2 §2): route delta the driver accepts to pick the
	// passenger up at their location. Zero for all other kinds.
	DetourAddedM float64
	DetourAddedS float64
}

// UpsertActiveDriver stores/refreshes the driver's active route. routeMEWKT is
// the measured `LINESTRING M` (M = cumulative seconds) from
// BuildMeasuredLineEWKT; pass "" to store NULL (matching then can't ETA-gate
// that driver). current_location is seeded to origin with a fresh timestamp so
// a just-set route reads as "driver at the start, now".
func UpsertActiveDriver(driverID, routeWKT, routeMEWKT string, originLat, originLng, destLat, destLng, corridorKm float64, seats int) error {
	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO active_drivers (driver_id, origin, destination, route, route_m, current_location, current_location_at, corridor_km, seats, status)
		VALUES (
			$1,
			ST_MakePoint($3, $2)::geography,
			ST_MakePoint($5, $4)::geography,
			ST_GeogFromText($6),
			CASE WHEN $7 = '' THEN NULL ELSE ST_GeomFromEWKT($7) END,
			ST_MakePoint($3, $2)::geography,
			NOW(),
			$8, $9, 'looking'
		)
		ON CONFLICT (driver_id) DO UPDATE SET
			origin              = EXCLUDED.origin,
			destination         = EXCLUDED.destination,
			route               = EXCLUDED.route,
			route_m             = EXCLUDED.route_m,
			current_location    = EXCLUDED.origin,
			current_location_at = NOW(),
			corridor_km         = EXCLUDED.corridor_km,
			seats               = EXCLUDED.seats,
			-- Re-broadcasting a route must NOT forget riders already on board:
			-- recompute occupancy from live requests instead of zeroing it,
			-- or a driver with 2 accepted riders could over-accept.
			seats_taken         = (
				SELECT COUNT(*)::int FROM ride_requests rr
				WHERE rr.driver_id = EXCLUDED.driver_id
				  AND rr.status IN ('accepted','in_progress')
			),
			status              = CASE WHEN (
				SELECT COUNT(*)::int FROM ride_requests rr
				WHERE rr.driver_id = EXCLUDED.driver_id
				  AND rr.status IN ('accepted','in_progress')
			) >= EXCLUDED.seats THEN 'full' ELSE 'looking' END,
			created_at          = NOW()
	`, driverID, originLat, originLng, destLat, destLng, routeWKT, routeMEWKT, corridorKm, seats)
	return err
}

// FindPassengersInCorridor finds passengers the driver can pick up without
// detouring. The carpool invariant: pickup and dropoff must both reduce to
// FORWARD points on the driver's existing route (driver_frac < pickup_frac <
// dropoff_frac), the off-route ends are short walks, and the on-route carry is
// long enough to be worth a stop.
//
// Gating (all tunables from config.C):
//   - Driver must have a FRESH current_location (no origin fallback) and a
//     measured route line (route_m) for ETA.
//   - ETA window: MatchEtaMinS ≤ drive-time(driver→pickup) ≤ MatchEtaMaxS.
//     The lower bound kills "about to pass / just passed"; the upper bound
//     defers premature notify (re-run as the driver approaches).
//   - Flat MatchMinAheadM along-route buffer (replaces the old route-length
//     fraction).
//   - Walk-in < cap (MatchWalkCapM, or MatchWalkLongM when the passenger
//     opted into longer walks).
//   - Same-way (when a destination is declared): dropoff projects FORWARD of
//     the pickup, the dropoff is genuinely on-route (off-route < corridor),
//     combined walk under cap, carry ≥ the road-class minimum (MatchMinCarryCityM,
//     or MatchMinCarryFastM when the pickup is on a fast road), and a coupling
//     guard carry ≥ MatchCarryK·(walk_in+walk_out) so a long walk can't eat a
//     short carry.
//   - No destination → "corridor_weak": pickup-only confidence (ETA + ahead +
//     walk), and only on non-fast roads (no flag-downs on a motorway).
//
// Road class is inferred from the OSRM-implied speed at the pickup (distance/
// time over a small window of the measured route line) — no external OSM data.
//
// On corridorKm: it only sets the bounding pre-filter radius and the on-route
// dropoff tolerance. It does NOT loosen match quality — every match is
// independently bounded by the walk caps (walk_in, and walk_in+walk_out) and
// the carry minimum. So a wide corridor (e.g. 6 km) just widens the candidate
// net; it cannot produce a far-off-route/long-walk match.
//
// Not done here: a heading/carriageway reject (#5) is intentionally omitted —
// with a single OSRM centerline it can't separate carriageways and a naive
// tangent-vs-bearing test wrongly drops valid riders on curved routes; it needs
// real driven geometry. The route-endpoint cases (#7/#12) and forward-reposition
// (#9) are handled outside this query. routeWKT is the bounding pre-filter;
// destLat/destLng are retained for signature stability.
func FindPassengersInCorridor(driverID, routeWKT string, corridorKm, destLat, destLng float64, limit int) ([]CorridorMatch, error) {
	return findPassengersInCorridor(driverID, routeWKT, corridorKm, destLat, destLng, limit, nil)
}

// FindCorridorMatchForPassenger runs the exact same corridor query restricted
// to ONE passenger. Used by the reverse trigger (a passenger appearing while
// the driver is parked) so a single new passenger doesn't require re-scoring
// the whole pool per candidate driver. Same gates, same semantics.
func FindCorridorMatchForPassenger(driverID, routeWKT string, corridorKm, destLat, destLng float64, passengerID string) ([]CorridorMatch, error) {
	return findPassengersInCorridor(driverID, routeWKT, corridorKm, destLat, destLng, 1, &passengerID)
}

func findPassengersInCorridor(driverID, routeWKT string, corridorKm, destLat, destLng float64, limit int, passengerID *string) ([]CorridorMatch, error) {
	corridorM := corridorKm * 1000
	rows, err := db.DB.Query(context.Background(), `
		-- CRS note (applies to every projection in this query):
		--
		-- ST_LineLocatePoint / ST_ClosestPoint are PLANAR. Run on raw 4326 they
		-- treat degrees as a flat grid, but a degree of longitude is only
		-- cos(lat) as long as a degree of latitude on the ground — ~0.64 at
		-- Krakow's latitude. That anisotropy skews the projection along the
		-- route: measured on a diagonal route, the "closest" point degree space
		-- picks sat 351 m further from the passenger than the true nearest
		-- point. That is over half the default 600 m walk cap, so it silently
		-- rejected valid matches and put pickup pins in the wrong place.
		--
		-- 3857 is conformal (locally equal scale in every direction), so the
		-- projection is unbiased. All fractions below are 3857 fractions and
		-- must stay mutually consistent; lengths transform back to geography to
		-- stay true metres, and points transform back to 4326 before touching
		-- route_m (which is a 4326 LINESTRING M).
		--
		-- Performance: route_p is the STORED projection (active_drivers.
		-- route_3857, kept by trigger) and rd/ri are MATERIALIZED. Postgres
		-- otherwise inlines these one-row CTEs into every candidate row and
		-- recomputes them there — the audit measured 2 061 ms → 419 ms at 5k
		-- active passengers with identical results.
		WITH rd AS MATERIALIZED (
			SELECT ad.route::geometry          AS route_geom,
			       ad.route_3857               AS route_p,
			       ad.route_m                  AS route_m,
			       ad.current_location::geometry AS cur_geom,
			       ST_Length(ad.route)         AS len_m
			FROM active_drivers ad
			WHERE ad.driver_id = $1
			  AND ad.current_location IS NOT NULL
			  AND ad.route_m IS NOT NULL
			  AND ad.route_3857 IS NOT NULL
			  AND ad.current_location_at > NOW() - make_interval(secs => $4::float8)
		),
		ri AS MATERIALIZED (
			SELECT rd.route_geom, rd.route_m, rd.len_m, rd.route_p,
			       ST_LineLocatePoint(rd.route_p, ST_Transform(rd.cur_geom, 3857)) AS dfrac,
			       ST_InterpolatePoint(rd.route_m, rd.cur_geom)                    AS dt
			FROM rd
		),
		c AS (
			SELECT u.id, u.name, COALESCE(u.fcm_token,'') AS fcm_token,
			       ap.location AS pax_loc,
			       COALESCE(ap.allow_long_walk, false) AS long_walk,
			       ri.route_geom, ri.route_p, ri.route_m, ri.len_m, ri.dfrac, ri.dt,
			       ST_LineLocatePoint(ri.route_p, pp.p)                                  AS pickup_frac,
			       ST_Transform(ST_ClosestPoint(ri.route_p, pp.p), 4326)::geography      AS pickup_geog,
			       d.geom AS dest_geom,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_LineLocatePoint(ri.route_p, ST_Transform(d.geom, 3857)) END AS dropoff_frac,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_Distance(d.geom::geography,
			                   ST_Transform(ST_ClosestPoint(ri.route_p,
			                       ST_Transform(d.geom, 3857)), 4326)::geography) END AS dropoff_off_m
			FROM active_passengers ap
			JOIN users u ON u.id = ap.passenger_id
			CROSS JOIN ri
			CROSS JOIN LATERAL (SELECT ST_Transform(ap.location::geometry, 3857) AS p) pp
			LEFT JOIN LATERAL (
			    SELECT ST_SetSRID(ST_MakePoint(ap.destination_lng, ap.destination_lat), 4326) AS geom
			    WHERE ap.destination_lat IS NOT NULL
			) d ON TRUE
			WHERE ST_DWithin(ap.location::geography, ST_GeogFromText($2), GREATEST($3::float8, $9::float8))
			  AND ap.updated_at > NOW() - INTERVAL '10 minutes'
			  -- Never match the driver's own passenger row (stale state after a
			  -- role switch would otherwise let a driver corridor-match themself).
			  AND u.id <> $1
			  -- Only a committed engagement removes a passenger from the pool.
			  -- An un-answered 'offered' row does NOT: multiple drivers may offer
			  -- concurrently (the passenger picks), otherwise the first broadcast
			  -- locks every corridor passenger for the full offer timeout.
			  AND NOT EXISTS (
			      SELECT 1 FROM ride_requests rr
			      WHERE rr.passenger_id = u.id
			        AND rr.status IN ('pending','accepted','in_progress')
			  )
			  -- Optional single-passenger restriction (reverse trigger); NULL
			  -- keeps the full-pool behavior byte-identical.
			  AND ($15::uuid IS NULL OR u.id = $15::uuid)
		),
		s AS (
			SELECT c.*,
			       ST_Distance(pax_loc, pickup_geog)                       AS walk_in_m,
			       -- Along-route METERS via ST_LineSubstring on the PROJECTED
			       -- line (the fractions above are 3857 fractions, so they must
			       -- be applied to the 3857 line), transformed back to
			       -- geography so the length is true metres rather than
			       -- Mercator-inflated ones.
			       SIGN(pickup_frac - dfrac) * ST_Length(ST_Transform(ST_LineSubstring(
			           route_p, LEAST(dfrac, pickup_frac), GREATEST(dfrac, pickup_frac)
			       ), 4326)::geography)                                    AS driver_to_pickup_m,
			       ST_InterpolatePoint(route_m, pickup_geog::geometry) - dt AS eta_pickup_s,
			       CASE WHEN dropoff_frac IS NOT NULL
			            THEN SIGN(dropoff_frac - pickup_frac) * ST_Length(ST_Transform(ST_LineSubstring(
			                route_p, LEAST(pickup_frac, dropoff_frac), GREATEST(pickup_frac, dropoff_frac)
			            ), 4326)::geography) END                           AS carry_m,
			       -- Road class inferred from OSRM-implied speed at the pickup:
			       -- distance/time over a small window on the measured line.
			       CASE WHEN (ST_InterpolatePoint(route_m, win.p2) - ST_InterpolatePoint(route_m, win.p1)) > 0
			            THEN ST_Distance(win.p1::geography, win.p2::geography)
			                 / (ST_InterpolatePoint(route_m, win.p2) - ST_InterpolatePoint(route_m, win.p1)) * 3.6
			            ELSE 0 END                                         AS pickup_speed_kmh,
			       ep.end_geom,
			       stop.stop_name, stop.stop_dist_m
			FROM c
			-- Speed window: same 3857 fractions, back to 4326 because both
			-- ST_InterpolatePoint (against route_m) and ::geography need it.
			CROSS JOIN LATERAL (
			    SELECT ST_Transform(ST_LineInterpolatePoint(route_p, GREATEST(pickup_frac - 0.005, 0)), 4326) AS p1,
			           ST_Transform(ST_LineInterpolatePoint(route_p, LEAST(pickup_frac + 0.005, 1)), 4326)    AS p2
			) win
			-- Route end + nearest transit stop: only meaningful for the
			-- partial-relay branch, NULL/harmless everywhere else.
			CROSS JOIN LATERAL (
			    SELECT ST_EndPoint(route_geom) AS end_geom
			) ep
			LEFT JOIN LATERAL (
			    SELECT ts.name AS stop_name,
			           ST_Distance(ts.location, ep.end_geom::geography) AS stop_dist_m
			    FROM transit_stops ts
			    WHERE ST_DWithin(ts.location, ep.end_geom::geography, $16::float8)
			    ORDER BY ST_Distance(ts.location, ep.end_geom::geography)
			    LIMIT 1
			) stop ON TRUE
		)
		SELECT id, name, fcm_token,
		       ST_Y(pickup_geog::geometry), ST_X(pickup_geog::geometry),
		       walk_in_m,
		       driver_to_pickup_m,
		       eta_pickup_s,
		       COALESCE(carry_m, 0),
		       COALESCE(dropoff_off_m, 0),
		       CASE WHEN dropoff_frac IS NULL THEN 'corridor_weak'
		            WHEN dropoff_frac >= 0.999 AND dropoff_off_m >= $3::float8 THEN 'partial_relay'
		            ELSE 'corridor' END AS match_kind,
		       ST_Y(end_geom), ST_X(end_geom),
		       COALESCE(stop_name, ''), COALESCE(stop_dist_m, 0)
		FROM s
		WHERE pickup_frac > dfrac
		  AND driver_to_pickup_m >= $5::float8
		  AND eta_pickup_s BETWEEN $6::float8 AND $7::float8
		  AND walk_in_m < (CASE WHEN long_walk THEN $9::float8 ELSE $8::float8 END)
		  AND (
		        -- Weak (no destination): pickup-only, and never on a fast road
		        -- (can't be flagged down on a motorway, no carry to justify it).
		        (dropoff_frac IS NULL AND pickup_speed_kmh < $13::float8)
		        OR (
		             dropoff_frac > pickup_frac
		             AND dropoff_off_m < $3::float8
		             AND (walk_in_m + COALESCE(dropoff_off_m, 0)) < (CASE WHEN long_walk THEN $9::float8 ELSE $8::float8 END)
		             -- City vs fast-road carry minimum (road class via speed).
		             AND carry_m >= (CASE WHEN pickup_speed_kmh >= $13::float8 THEN $14::float8 ELSE $10::float8 END)
		             AND carry_m >= $11::float8 * (walk_in_m + COALESCE(dropoff_off_m, 0))
		        )
		        -- Partial relay (Round 2 §1): destination projects PAST the
		        -- route end (frac clamps to 1, dest genuinely off-route).
		        -- Leg-1 gates still hold: forward pickup / ETA / walk-in are
		        -- in the global WHERE above; carry (pickup → route end) must
		        -- meet the road-class minimum and the coupling guard
		        -- (walk_out excluded — the passenger continues, not walks).
		        -- Gated on continuation feasibility at the route end: a
		        -- transit stop within GTFSStopRadiusM OR enough other looking
		        -- drivers around the dropoff for a realistic second leg.
		        -- Neither → rejected exactly as before this branch existed.
		        OR (
		             dropoff_frac >= 0.999
		             AND dropoff_off_m >= $3::float8
		             AND carry_m >= (CASE WHEN pickup_speed_kmh >= $13::float8 THEN $14::float8 ELSE $10::float8 END)
		             AND carry_m >= $11::float8 * walk_in_m
		             AND (
		                  stop_name IS NOT NULL
		                  OR (SELECT COUNT(*) FROM active_drivers ad2
		                      WHERE ad2.driver_id <> $1
		                        AND ad2.status = 'looking'
		                        AND ad2.route IS NOT NULL
		                        AND ad2.created_at > NOW() - INTERVAL '30 minutes'
		                        AND ST_DWithin(ad2.route::geography, end_geom::geography, $3::float8)
		                     ) >= $17::int
		             )
		        )
		  )
		ORDER BY eta_pickup_s ASC
		LIMIT $12
	`,
		driverID,                     // $1
		routeWKT,                     // $2  (bounding pre-filter)
		corridorM,                    // $3  on-route dropoff tolerance
		config.C.MatchLocFreshS,      // $4
		config.C.MatchMinAheadM,      // $5
		config.C.MatchEtaMinS,        // $6
		config.C.MatchEtaMaxS,        // $7
		config.C.MatchWalkCapM,       // $8
		config.C.MatchWalkLongM,      // $9
		config.C.MatchMinCarryCityM,  // $10
		config.C.MatchCarryK,         // $11
		limit,                        // $12
		config.C.MatchFastRoadKmh,    // $13
		config.C.MatchMinCarryFastM,  // $14
		passengerID,                  // $15 (nil → no restriction)
		config.C.GTFSStopRadiusM,     // $16
		config.C.MatchRelayMinDrivers, // $17
	)
	if err != nil {
		return nil, fmt.Errorf("corridor query: %w", err)
	}
	defer rows.Close()

	var matches []CorridorMatch
	for rows.Next() {
		var m CorridorMatch
		if err := rows.Scan(
			&m.User.ID, &m.User.Name, &m.User.FCMToken,
			&m.PickupLat, &m.PickupLng,
			&m.WalkM, &m.DriverToPickupM, &m.EtaPickupS,
			&m.CarryM, &m.WalkOutM,
			&m.MatchKind,
			&m.DropoffLat, &m.DropoffLng,
			&m.TransitStopName, &m.TransitStopDistM,
		); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// NearMissCandidate is a passenger who failed corridor matching ONLY on the
// walk-in cap (Round 2 §2): every other gate — forward pickup, ETA window,
// forward on-route dropoff, carry minimums — passes. These are the sole
// inputs to the micro-detour evaluation; nothing else ever reaches it.
type NearMissCandidate struct {
	User       models.User
	PaxLat     float64 // raw passenger location — the via-point / new pickup
	PaxLng     float64
	WalkM      float64 // the walk that failed the cap (for logging)
	CarryM     float64
	EtaPickupS float64
}

// FindNearMissCandidates returns walk-only corridor failures for this driver,
// closest-ETA first, capped at limit. Same gate expressions as
// findPassengersInCorridor with two deliberate differences, both documented
// in MATCHING.md terms: walk-in must be BETWEEN the cap and
// MatchDetourNearMissM (a detour moves the pickup to the passenger, so the
// walk disappears), and the carry coupling uses walk_out alone (walk-in is
// zero after the detour).
func FindNearMissCandidates(driverID, routeWKT string, corridorKm float64, limit int) ([]NearMissCandidate, error) {
	corridorM := corridorKm * 1000
	rows, err := db.DB.Query(context.Background(), `
		-- Projections go through 3857, not raw 4326 — see the CRS note in
		-- findPassengersInCorridor. The gates here must stay numerically
		-- identical to that query's or "near miss" stops meaning "failed the
		-- walk cap and nothing else".
		-- Stored projection + MATERIALIZED, as in findPassengersInCorridor
		-- (measured 624 ms → 117 ms at 5k active passengers).
		WITH rd AS MATERIALIZED (
			SELECT ad.route::geometry            AS route_geom,
			       ad.route_3857                 AS route_p,
			       ad.route_m                    AS route_m,
			       ad.current_location::geometry AS cur_geom
			FROM active_drivers ad
			WHERE ad.driver_id = $1
			  AND ad.current_location IS NOT NULL
			  AND ad.route_m IS NOT NULL
			  AND ad.route_3857 IS NOT NULL
			  AND ad.current_location_at > NOW() - make_interval(secs => $4::float8)
		),
		ri AS MATERIALIZED (
			SELECT rd.route_geom, rd.route_m, rd.route_p,
			       ST_LineLocatePoint(rd.route_p, ST_Transform(rd.cur_geom, 3857)) AS dfrac,
			       ST_InterpolatePoint(rd.route_m, rd.cur_geom)                    AS dt
			FROM rd
		),
		c AS (
			SELECT u.id, u.name, COALESCE(u.fcm_token,'') AS fcm_token,
			       ap.location AS pax_loc,
			       ST_Y(ap.location::geometry) AS pax_lat,
			       ST_X(ap.location::geometry) AS pax_lng,
			       COALESCE(ap.allow_long_walk, false) AS long_walk,
			       ri.route_geom, ri.route_p, ri.route_m, ri.dfrac, ri.dt,
			       ST_LineLocatePoint(ri.route_p, pp.p)                             AS pickup_frac,
			       ST_Transform(ST_ClosestPoint(ri.route_p, pp.p), 4326)::geography AS pickup_geog,
			       d.geom AS dest_geom,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_LineLocatePoint(ri.route_p, ST_Transform(d.geom, 3857)) END AS dropoff_frac,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_Distance(d.geom::geography,
			                   ST_Transform(ST_ClosestPoint(ri.route_p,
			                       ST_Transform(d.geom, 3857)), 4326)::geography) END AS dropoff_off_m
			FROM active_passengers ap
			JOIN users u ON u.id = ap.passenger_id
			CROSS JOIN ri
			CROSS JOIN LATERAL (SELECT ST_Transform(ap.location::geometry, 3857) AS p) pp
			LEFT JOIN LATERAL (
			    SELECT ST_SetSRID(ST_MakePoint(ap.destination_lng, ap.destination_lat), 4326) AS geom
			    WHERE ap.destination_lat IS NOT NULL
			) d ON TRUE
			WHERE ST_DWithin(ap.location::geography, ST_GeogFromText($2), GREATEST($3::float8, $10::float8))
			  AND ap.updated_at > NOW() - INTERVAL '10 minutes'
			  AND u.id <> $1
			  AND NOT EXISTS (
			      SELECT 1 FROM ride_requests rr
			      WHERE rr.passenger_id = u.id
			        AND rr.status IN ('pending','accepted','in_progress')
			  )
		),
		s AS (
			SELECT c.*,
			       ST_Distance(pax_loc, pickup_geog) AS walk_in_m,
			       SIGN(pickup_frac - dfrac) * ST_Length(ST_Transform(ST_LineSubstring(
			           route_p, LEAST(dfrac, pickup_frac), GREATEST(dfrac, pickup_frac)
			       ), 4326)::geography)                                     AS driver_to_pickup_m,
			       ST_InterpolatePoint(route_m, pickup_geog::geometry) - dt AS eta_pickup_s,
			       CASE WHEN dropoff_frac IS NOT NULL
			            THEN SIGN(dropoff_frac - pickup_frac) * ST_Length(ST_Transform(ST_LineSubstring(
			                route_p, LEAST(pickup_frac, dropoff_frac), GREATEST(pickup_frac, dropoff_frac)
			            ), 4326)::geography) END                            AS carry_m
			FROM c
		)
		SELECT id, name, fcm_token, pax_lat, pax_lng,
		       walk_in_m, COALESCE(carry_m, 0), eta_pickup_s
		FROM s
		WHERE pickup_frac > dfrac
		  AND driver_to_pickup_m >= $5::float8
		  AND eta_pickup_s BETWEEN $6::float8 AND $7::float8
		  -- The ONE inverted gate: walk-in FAILS the cap but stays inside
		  -- the near-miss outer bound.
		  AND walk_in_m >= (CASE WHEN long_walk THEN $9::float8 ELSE $8::float8 END)
		  AND walk_in_m < $10::float8
		  -- Destination branch only — no weak matches, no relay here.
		  AND dropoff_frac IS NOT NULL
		  AND dropoff_frac > pickup_frac
		  AND dropoff_off_m < $3::float8
		  AND carry_m >= $11::float8
		  AND carry_m >= $12::float8 * COALESCE(dropoff_off_m, 0)
		ORDER BY eta_pickup_s ASC
		LIMIT $13
	`,
		driverID,                     // $1
		routeWKT,                     // $2
		corridorM,                    // $3
		config.C.MatchLocFreshS,      // $4
		config.C.MatchMinAheadM,      // $5
		config.C.MatchEtaMinS,        // $6
		config.C.MatchEtaMaxS,        // $7
		config.C.MatchWalkCapM,       // $8
		config.C.MatchWalkLongM,      // $9
		config.C.MatchDetourNearMissM, // $10
		config.C.MatchMinCarryCityM,  // $11
		config.C.MatchCarryK,         // $12
		limit,                        // $13
	)
	if err != nil {
		return nil, fmt.Errorf("near-miss query: %w", err)
	}
	defer rows.Close()

	var out []NearMissCandidate
	for rows.Next() {
		var n NearMissCandidate
		if err := rows.Scan(
			&n.User.ID, &n.User.Name, &n.User.FCMToken,
			&n.PaxLat, &n.PaxLng,
			&n.WalkM, &n.CarryM, &n.EtaPickupS,
		); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func UpsertActivePassenger(passengerID string, lat, lng float64) error {
	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO active_passengers (passenger_id, location, updated_at)
		VALUES ($1, ST_MakePoint($3, $2)::geography, NOW())
		ON CONFLICT (passenger_id) DO UPDATE SET
			location   = EXCLUDED.location,
			updated_at = NOW()
	`, passengerID, lat, lng)
	return err
}

func DeleteActivePassenger(passengerID string) error {
	_, err := db.DB.Exec(context.Background(),
		`DELETE FROM active_passengers WHERE passenger_id = $1`, passengerID)
	return err
}

// CreateRideRequest creates a passenger-initiated request (status 'pending',
// awaiting the driver). Thin wrapper over CreateRideRequestWithStatus.
func CreateRideRequest(driverID, passengerID string, pickupLat, pickupLng float64) (string, bool, error) {
	return CreateRideRequestWithStatus(driverID, passengerID, pickupLat, pickupLng, "pending")
}

// CreateRideRequestWithStatus creates a new ride_request row at the given
// initial status OR returns the existing active row's id without mutating it.
// Idempotent — safe for both the driver-initiated corridor match (status
// 'offered') and the passenger-initiated RequestDriver flow ('pending'). The
// partial unique index `uniq_active_ride_request` (db/postgres.go) scopes
// uniqueness to the active set (offered/pending/accepted/in_progress), so
// cancelled/declined/expired history rows coexist freely.
//
// Returns (id, isNew, error). isNew=false means an active row already existed —
// the caller should NOT emit a fresh notification.
func CreateRideRequestWithStatus(driverID, passengerID string, pickupLat, pickupLng float64, status string) (string, bool, error) {
	return CreateRideRequestFull(driverID, passengerID, pickupLat, pickupLng, status, RideRequestDetails{MatchKind: "corridor"})
}

// RideRequestDetails carries the Round-2 match labeling persisted on the
// ride_requests row: the match kind, the relay dropoff (partial_relay), and
// the disclosed detour delta (micro_detour).
type RideRequestDetails struct {
	MatchKind    string
	DropoffLat   *float64
	DropoffLng   *float64
	DetourAddedM *float64
	DetourAddedS *float64
}

// CreateRideRequestFull is CreateRideRequestWithStatus plus match labeling.
func CreateRideRequestFull(driverID, passengerID string, pickupLat, pickupLng float64, status string, d RideRequestDetails) (string, bool, error) {
	if d.MatchKind == "" {
		d.MatchKind = "corridor"
	}
	ctx := context.Background()
	var id string
	err := db.DB.QueryRow(ctx, `
		INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status,
		                           match_kind, dropoff_location, detour_added_m, detour_added_s)
		VALUES ($1, $2, ST_MakePoint($4, $3)::geography, $5, $6,
		        CASE WHEN $7::float8 IS NULL THEN NULL
		             ELSE ST_MakePoint($8::float8, $7::float8)::geography END,
		        $9, $10)
		ON CONFLICT (driver_id, passenger_id) WHERE status IN ('offered','pending','accepted','in_progress') DO NOTHING
		RETURNING id
	`, driverID, passengerID, pickupLat, pickupLng, status,
		d.MatchKind, d.DropoffLat, d.DropoffLng, d.DetourAddedM, d.DetourAddedS).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	// Active row exists — return it untouched.
	err = db.DB.QueryRow(ctx, `
		SELECT id FROM ride_requests
		WHERE driver_id = $1 AND passenger_id = $2
		  AND status IN ('offered','pending','accepted','in_progress')
		LIMIT 1
	`, driverID, passengerID).Scan(&id)
	if err != nil {
		return "", false, err
	}
	return id, false, nil
}

// GetDriverRequests returns the driver's *active* requests (pending+accepted),
// joined with the passenger's name and rating so the client never falls back to
// "Passenger <uuid>…" placeholder text. Cancelled/declined history is excluded.
func GetDriverRequests(driverID string) ([]models.RideRequest, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT rr.id, rr.driver_id, rr.passenger_id,
		       COALESCE(ST_Y(rr.pickup_location::geometry), 0),
		       COALESCE(ST_X(rr.pickup_location::geometry), 0),
		       rr.status, rr.created_at,
		       u.name, COALESCE(u.photo_url, ''),
		       -- Scalar subqueries rather than a join + GROUP BY: the drop-off
		       -- lateral below cannot be grouped over without listing every one
		       -- of its outputs, and idx_ratings_rated makes these index hits.
		       (SELECT COALESCE(AVG(stars), 0) FROM ratings WHERE rated_id = rr.passenger_id),
		       (SELECT COUNT(*) FROM ratings WHERE rated_id = rr.passenger_id),
		       COALESCE(rr.pickup_code, ''), rr.picked_up_at,
		       rr.match_kind, rr.detour_added_m, rr.detour_added_s,
		       d.lat, d.lng, d.carry_m
		FROM ride_requests rr
		JOIN users u ON u.id = rr.passenger_id
		LEFT JOIN active_drivers ad
		       ON ad.driver_id = rr.driver_id AND ad.route IS NOT NULL
		LEFT JOIN active_passengers ap ON ap.passenger_id = rr.passenger_id
		-- Where this passenger gets OUT, and how far they ride, so the driver
		-- can judge a pending request instead of accepting blind.
		--
		-- Computed on read, not persisted: requests are created from three
		-- paths (corridor push, RequestDriver, micro-detour) and only the first
		-- has this geometry to hand, so one lateral here covers all three
		-- uniformly and stays correct if a route is re-planned.
		--
		-- Fractions are 3857 fractions throughout — degree-space projection is
		-- anisotropic and picks the wrong point on any route that is not
		-- axis-aligned. Same rule as findPassengersInCorridor; these must agree
		-- or the driver is shown a drop-off the matcher never chose.
		LEFT JOIN LATERAL (
			SELECT ST_Y(g.pt::geometry) AS lat,
			       ST_X(g.pt::geometry) AS lng,
			       ST_Length(ST_Transform(ST_LineSubstring(
			           rp.route_p,
			           LEAST(f.pickup_frac, f.dropoff_frac),
			           GREATEST(f.pickup_frac, f.dropoff_frac)
			       ), 4326)::geography) AS carry_m
			FROM (SELECT ad.route_3857 AS route_p) rp
			CROSS JOIN LATERAL (
				-- partial_relay stores an explicit relay drop-off; everything
				-- else projects the passenger's declared destination onto the
				-- driver's existing route. NULL when they declared none
				-- (corridor_weak) — nothing to project, so no drop-off shown.
				SELECT COALESCE(
				    rr.dropoff_location,
				    CASE WHEN ap.destination_lat IS NOT NULL
				         THEN ST_Transform(ST_ClosestPoint(rp.route_p,
				                  ST_Transform(ST_SetSRID(ST_MakePoint(
				                      ap.destination_lng, ap.destination_lat), 4326), 3857)
				              ), 4326)::geography
				    END) AS pt
			) g
			CROSS JOIN LATERAL (
				SELECT ST_LineLocatePoint(rp.route_p,
				           ST_Transform(rr.pickup_location::geometry, 3857)) AS pickup_frac,
				       ST_LineLocatePoint(rp.route_p,
				           ST_Transform(g.pt::geometry, 3857))               AS dropoff_frac
			) f
			WHERE g.pt IS NOT NULL
		) d ON TRUE
		WHERE rr.driver_id = $1
		  AND rr.status IN ('pending','accepted','in_progress')
		ORDER BY rr.created_at DESC
	`, driverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []models.RideRequest
	for rows.Next() {
		var r models.RideRequest
		if err := rows.Scan(
			&r.ID, &r.DriverID, &r.PassengerID,
			&r.PickupLat, &r.PickupLng,
			&r.Status, &r.CreatedAt,
			&r.PassengerName, &r.PassengerPhotoURL,
			&r.PassengerAvgRating, &r.PassengerRatingCount,
			&r.PickupCode, &r.PickedUpAt,
			&r.MatchKind, &r.DetourAddedM, &r.DetourAddedS,
			&r.DropoffLat, &r.DropoffLng, &r.CarryM,
		); err != nil {
			return nil, err
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// UpdateRideRequestStatus flips a passenger-owned request to `status`, but
// only from pre-seat states ('offered'/'pending'). Accepted and in-progress
// rides must go through the cancel endpoints, which also restore the
// driver's seat count — flipping them here would strand capacity.
func UpdateRideRequestStatus(requestID, passengerID, status string) (*models.RideRequest, error) {
	var r models.RideRequest
	err := db.DB.QueryRow(context.Background(), `
		UPDATE ride_requests SET status = $1
		WHERE id = $2 AND passenger_id = $3
		  AND status IN ('offered','pending')
		RETURNING id, driver_id, passenger_id,
		          COALESCE(ST_Y(pickup_location::geometry), 0),
		          COALESCE(ST_X(pickup_location::geometry), 0),
		          status, created_at
	`, status, requestID, passengerID).Scan(
		&r.ID, &r.DriverID, &r.PassengerID, &r.PickupLat, &r.PickupLng, &r.Status, &r.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func UpdateDriverStatus(driverID, status string) error {
	_, err := db.DB.Exec(context.Background(),
		`UPDATE active_drivers SET status = $1 WHERE driver_id = $2`, status, driverID)
	return err
}

func DeleteActiveDriver(driverID string) error {
	_, err := db.DB.Exec(context.Background(),
		`DELETE FROM active_drivers WHERE driver_id = $1`, driverID)
	return err
}

// Pre-match privacy. Anyone signed in can see looking drivers, and a driver's
// route usually starts at home and ends at work. So before a ride is accepted:
//   - start/end points are snapped to a PrivacyGridDeg grid (~550 m N–S,
//     ~360 m E–W at Kraków's latitude), enough to judge "same way";
//   - the drawn route loses PrivacyTrimM at each end, so its endpoints don't
//     reveal the exact addresses either;
//   - only the first name is shown and the plate is withheld until accept.
const (
	PrivacyGridDeg = 0.005
	PrivacyTrimM   = 300.0 // mirrored in the active_drivers_derive_route trigger
)

// SnapToPrivacyGrid rounds a coordinate to the PrivacyGridDeg grid, matching
// what ST_SnapToGrid does for the nearby-drivers query.
func SnapToPrivacyGrid(v float64) float64 {
	return math.Round(v/PrivacyGridDeg) * PrivacyGridDeg
}

type NearbyDriver struct {
	DriverID   string `json:"driver_id"`
	DriverName string `json:"driver_name"` // first name only (pre-match)
	// Origin/destination snapped to PrivacyGridDeg — never exact.
	OriginLat float64 `json:"origin_lat"`
	OriginLng float64 `json:"origin_lng"`
	DestLat   float64 `json:"dest_lat"`
	DestLng   float64 `json:"dest_lng"`
	CorridorKm float64 `json:"corridor_km"`
	Seats      int     `json:"seats"`
	CarMake    string  `json:"car_make"`
	CarModel   string  `json:"car_model"`
	CarColor   string  `json:"car_color"`
	// NOTE: car_plate is deliberately absent.
	//
	// This endpoint is callable by any authenticated user for arbitrary
	// coordinates, so everything in it is effectively public to the user base.
	// Name + licence plate + route pins a specific person to a specific car.
	// The plate is only actually needed once a ride is accepted and the
	// passenger has to identify the arriving vehicle — driverRespondAccept
	// already delivers it on that ride's WS payload. Make/model/colour stay,
	// which is what the browse UI shows.

	// RouteWKT is TRIMMED by PrivacyTrimM at both ends and SIMPLIFIED (see the
	// query): enough to draw "is this driver going my way?" on the map,
	// without publishing a metre-accurate track or the exact endpoints.
	RouteWKT string `json:"route_wkt"`
	// LikelyMatch tells the passenger which listed drivers the corridor-push
	// algorithm would consider same-way (forward projection + walk cap +
	// carry gates) so a manual pick from the map is informed, not guesswork.
	// It is a HINT: it skips the ETA window and road-class refinement, so the
	// authoritative decision still happens at request/offer time. False when
	// the caller has no location/destination on file.
	LikelyMatch bool `json:"likely_match"`
	// WalkM is the caller's off-route walk to this driver's route (-1 when
	// the caller has no active location). CarryM is the on-route carried
	// distance toward the caller's declared destination (0 when unknown).
	WalkM  float64 `json:"walk_m"`
	CarryM float64 `json:"carry_m"`
	// Driver reputation, so the passenger's candidate list can be sorted and
	// judged without opening each driver in turn. Unlike the licence plate
	// above, name + rating is the same pairing every marketplace shows before
	// booking, and it does not help anyone identify a specific vehicle.
	// Count is 0 for a driver nobody has rated yet — render "new", not "0.0".
	DriverAvgRating   float64 `json:"driver_avg_rating"`
	DriverRatingCount int     `json:"driver_rating_count"`
}

// GetNearbyDrivers returns active drivers whose origin is within radiusKm of (lat, lng).
// When destRadiusKm > 0, only drivers whose route passes within destRadiusKm of (destLat, destLng) are returned.
// callerID excludes the caller themselves so a user who is both driver and passenger
// (after a role switch with stale state) never sees their own car; it is also the
// active_passengers row the same-way hint (likely_match) is computed against.
func GetNearbyDrivers(lat, lng, radiusKm, destLat, destLng, destRadiusKm float64, callerID string) ([]NearbyDriver, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT ad.driver_id,
		       split_part(u.name, ' ', 1),
		       ST_Y(ST_SnapToGrid(ad.origin::geometry, $13::float8)),
		       ST_X(ST_SnapToGrid(ad.origin::geometry, $13::float8)),
		       ST_Y(ST_SnapToGrid(ad.destination::geometry, $13::float8)),
		       ST_X(ST_SnapToGrid(ad.destination::geometry, $13::float8)),
		       ad.corridor_km, ad.seats,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''),
		       -- Trimmed by PrivacyTrimM at each end and simplified ~50 m,
		       -- precomputed by trigger at SetRoute (db/postgres.go) instead of
		       -- on every 10-second poll of every passenger.
		       COALESCE(ad.route_public_wkt, ''),
		       COALESCE(lm.likely, false),
		       COALESCE(lm.walk_m, -1),
		       COALESCE(lm.carry_m, 0),
		       -- Scalar subqueries, not a GROUP BY: this select list is wide
		       -- and already carries a LEFT JOIN LATERAL, so regrouping it to
		       -- add two aggregates is the riskier edit. idx_ratings_rated
		       -- makes both index lookups.
		       (SELECT COALESCE(AVG(stars), 0) FROM ratings WHERE rated_id = ad.driver_id),
		       (SELECT COUNT(*) FROM ratings WHERE rated_id = ad.driver_id)
		FROM active_drivers ad
		JOIN users u ON u.id = ad.driver_id
		LEFT JOIN active_passengers ap ON ap.passenger_id = $7
		-- Same-way hint per driver, mirroring the corridor-push conditions
		-- (forward pickup, walk cap, forward on-route dropoff, carry minimum
		-- + coupling). Deliberately no ETA window / road-class here — see
		-- NearbyDriver.LikelyMatch.
		LEFT JOIN LATERAL (
			SELECT s.walk_in_m AS walk_m,
			       COALESCE(s.carry_m, 0) AS carry_m,
			       (s.pickup_frac > s.dfrac
			        AND s.walk_in_m < (CASE WHEN COALESCE(ap.allow_long_walk, false) THEN $9::float8 ELSE $8::float8 END)
			        AND s.dropoff_frac IS NOT NULL
			        AND s.dropoff_frac > s.pickup_frac
			        AND s.dropoff_off_m < ad.corridor_km * 1000
			        AND s.carry_m >= $10::float8
			        AND s.carry_m >= $11::float8 * (s.walk_in_m + COALESCE(s.dropoff_off_m, 0))
			       ) AS likely
			FROM (
				SELECT ST_LineLocatePoint(rg.g, rg.pax_p) AS pickup_frac,
				       ST_LineLocatePoint(rg.g,
				           ST_Transform(CASE WHEN ad.current_location IS NOT NULL
				                 AND ad.current_location_at > NOW() - make_interval(secs => $12::float8)
				                THEN ad.current_location::geometry
				                ELSE ad.origin::geometry END, 3857)) AS dfrac,
				       ST_Distance(ap.location,
				           ST_Transform(ST_ClosestPoint(rg.g, rg.pax_p), 4326)::geography) AS walk_in_m,
				       dd.dropoff_frac, dd.dropoff_off_m,
				       CASE WHEN dd.dropoff_frac IS NOT NULL THEN
				           SIGN(dd.dropoff_frac - ST_LineLocatePoint(rg.g, rg.pax_p))
				           * ST_Length(ST_Transform(ST_LineSubstring(rg.g,
				                 LEAST(ST_LineLocatePoint(rg.g, rg.pax_p), dd.dropoff_frac),
				                 GREATEST(ST_LineLocatePoint(rg.g, rg.pax_p), dd.dropoff_frac)
				             ), 4326)::geography)
				       END AS carry_m
				-- 3857 like the corridor query: this hint is meant to predict
				-- what the real matcher will decide, so it has to project the
				-- same way or it will disagree with it on diagonal routes.
				FROM (SELECT ad.route_3857                             AS g,
				             ST_Transform(ap.location::geometry, 3857) AS pax_p) rg
				LEFT JOIN LATERAL (
					SELECT ST_LineLocatePoint(rg.g, ST_Transform(dp.p, 3857)) AS dropoff_frac,
					       ST_Distance(dp.p::geography,
					           ST_Transform(ST_ClosestPoint(rg.g,
					               ST_Transform(dp.p, 3857)), 4326)::geography) AS dropoff_off_m
					FROM (SELECT ST_SetSRID(ST_MakePoint(ap.destination_lng, ap.destination_lat), 4326) AS p
					      WHERE ap.destination_lat IS NOT NULL) dp
				) dd ON TRUE
			) s
			WHERE ap.passenger_id IS NOT NULL AND ad.route_3857 IS NOT NULL
			  -- likely_match requires walk_in < the walk cap (≤ $9), so a
			  -- route farther than $9 can never be "likely": skip the
			  -- per-driver geometry for it (index-assisted, measured 2.6×
			  -- faster at 5k with the stored WKT). walk_m/carry_m then read
			  -- -1/0 for those drivers, which the app treats as unknown.
			  AND ST_DWithin(ad.route, ap.location, $9::float8)
		) lm ON TRUE
		WHERE ST_DWithin(
			ad.origin::geography,
			ST_MakePoint($2, $1)::geography,
			$3 * 1000
		)
		AND ($6::float = 0 OR ST_DWithin(
			ad.route::geography,
			ST_MakePoint($5, $4)::geography,
			$6 * 1000
		))
		AND ad.status = 'looking'
		AND ad.created_at > NOW() - INTERVAL '30 minutes'
		AND ad.driver_id <> $7
	`, lat, lng, radiusKm, destLat, destLng, destRadiusKm, callerID,
		config.C.MatchWalkCapM,      // $8
		config.C.MatchWalkLongM,     // $9
		config.C.MatchMinCarryCityM, // $10
		config.C.MatchCarryK,        // $11
		config.C.MatchLocFreshS,     // $12
		PrivacyGridDeg,              // $13
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var drivers []NearbyDriver
	for rows.Next() {
		var d NearbyDriver
		if err := rows.Scan(
			&d.DriverID, &d.DriverName,
			&d.OriginLat, &d.OriginLng,
			&d.DestLat, &d.DestLng,
			&d.CorridorKm, &d.Seats,
			&d.CarMake, &d.CarModel, &d.CarColor,
			&d.RouteWKT,
			&d.LikelyMatch, &d.WalkM, &d.CarryM,
			&d.DriverAvgRating, &d.DriverRatingCount,
		); err != nil {
			return nil, err
		}
		drivers = append(drivers, d)
	}
	return drivers, rows.Err()
}

func GetActiveDriver(driverID string) (*models.ActiveDriver, error) {
	var d models.ActiveDriver
	err := db.DB.QueryRow(context.Background(), `
		SELECT id, driver_id,
		       ST_Y(origin::geometry), ST_X(origin::geometry),
		       ST_Y(destination::geometry), ST_X(destination::geometry),
		       corridor_km, seats, status, created_at
		FROM active_drivers WHERE driver_id = $1
	`, driverID).Scan(
		&d.ID, &d.DriverID,
		&d.OriginLat, &d.OriginLng,
		&d.DestLat, &d.DestLng,
		&d.CorridorKm, &d.Seats, &d.Status, &d.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}
