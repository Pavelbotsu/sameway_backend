package services

import (
	"context"
	"errors"
	"fmt"

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
	MatchKind       string  // "corridor" | "corridor_weak"
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
			seats_taken         = 0,
			status              = 'looking',
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
//     combined walk under cap, carry ≥ MatchMinCarryCityM, and a coupling
//     guard carry ≥ MatchCarryK·(walk_in+walk_out) so a long walk can't eat a
//     short carry.
//   - No destination → "corridor_weak": pickup-only confidence (ETA + ahead +
//     walk), no carry guarantee.
//
// Note (Phase 2): bearing/heading reject, the city/fast carry split by OSM
// road class, and the route-endpoint cases (ride-to-end #7, walk-to-start #12)
// are added once road-class + bearing land. routeWKT is used for the bounding
// pre-filter; destLat/destLng are retained for signature stability and are no
// longer part of the predicate (the same-way test uses the passenger's own
// declared destination).
func FindPassengersInCorridor(driverID, routeWKT string, corridorKm, destLat, destLng float64, limit int) ([]CorridorMatch, error) {
	corridorM := corridorKm * 1000
	rows, err := db.DB.Query(context.Background(), `
		WITH ri AS (
			SELECT ad.route::geometry AS route_geom,
			       ad.route_m          AS route_m,
			       ST_Length(ad.route) AS len_m,
			       ST_LineLocatePoint(ad.route::geometry, ad.current_location::geometry) AS dfrac,
			       ST_InterpolatePoint(ad.route_m, ad.current_location::geometry)        AS dt
			FROM active_drivers ad
			WHERE ad.driver_id = $1
			  AND ad.current_location IS NOT NULL
			  AND ad.route_m IS NOT NULL
			  AND ad.current_location_at > NOW() - make_interval(secs => $4::float8)
		),
		c AS (
			SELECT u.id, u.name, COALESCE(u.fcm_token,'') AS fcm_token,
			       ap.location AS pax_loc,
			       COALESCE(ap.allow_long_walk, false) AS long_walk,
			       ri.route_geom, ri.route_m, ri.len_m, ri.dfrac, ri.dt,
			       ST_LineLocatePoint(ri.route_geom, ap.location::geometry)        AS pickup_frac,
			       ST_ClosestPoint(ri.route_geom, ap.location::geometry)::geography AS pickup_geog,
			       d.geom AS dest_geom,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_LineLocatePoint(ri.route_geom, d.geom) END AS dropoff_frac,
			       CASE WHEN d.geom IS NOT NULL
			            THEN ST_Distance(d.geom::geography,
			                   ST_ClosestPoint(ri.route_geom, d.geom)::geography) END AS dropoff_off_m
			FROM active_passengers ap
			JOIN users u ON u.id = ap.passenger_id
			CROSS JOIN ri
			LEFT JOIN LATERAL (
			    SELECT ST_SetSRID(ST_MakePoint(ap.destination_lng, ap.destination_lat), 4326) AS geom
			    WHERE ap.destination_lat IS NOT NULL
			) d ON TRUE
			WHERE ST_DWithin(ap.location::geography, ST_GeogFromText($2), GREATEST($3::float8, $9::float8))
			  AND ap.updated_at > NOW() - INTERVAL '10 minutes'
			  AND NOT EXISTS (
			      SELECT 1 FROM ride_requests rr
			      WHERE rr.passenger_id = u.id
			        AND rr.status IN ('pending','accepted','in_progress')
			  )
		),
		s AS (
			SELECT *,
			       ST_Distance(pax_loc, pickup_geog)                       AS walk_in_m,
			       (pickup_frac - dfrac) * len_m                           AS driver_to_pickup_m,
			       ST_InterpolatePoint(route_m, pickup_geog::geometry) - dt AS eta_pickup_s,
			       CASE WHEN dropoff_frac IS NOT NULL
			            THEN (dropoff_frac - pickup_frac) * len_m END      AS carry_m
			FROM c
		)
		SELECT id, name, fcm_token,
		       ST_Y(pickup_geog::geometry), ST_X(pickup_geog::geometry),
		       walk_in_m,
		       driver_to_pickup_m,
		       eta_pickup_s,
		       COALESCE(carry_m, 0),
		       COALESCE(dropoff_off_m, 0),
		       CASE WHEN dropoff_frac IS NULL THEN 'corridor_weak' ELSE 'corridor' END AS match_kind
		FROM s
		WHERE pickup_frac > dfrac
		  AND driver_to_pickup_m >= $5::float8
		  AND eta_pickup_s BETWEEN $6::float8 AND $7::float8
		  AND walk_in_m < (CASE WHEN long_walk THEN $9::float8 ELSE $8::float8 END)
		  AND (
		        dropoff_frac IS NULL
		        OR (
		             dropoff_frac > pickup_frac
		             AND dropoff_off_m < $3::float8
		             AND (walk_in_m + COALESCE(dropoff_off_m, 0)) < (CASE WHEN long_walk THEN $9::float8 ELSE $8::float8 END)
		             AND carry_m >= $10::float8
		             AND carry_m >= $11::float8 * (walk_in_m + COALESCE(dropoff_off_m, 0))
		        )
		  )
		ORDER BY eta_pickup_s ASC
		LIMIT $12
	`,
		driverID,                 // $1
		routeWKT,                 // $2  (bounding pre-filter)
		corridorM,                // $3  on-route dropoff tolerance
		config.C.MatchLocFreshS,  // $4
		config.C.MatchMinAheadM,  // $5
		config.C.MatchEtaMinS,    // $6
		config.C.MatchEtaMaxS,    // $7
		config.C.MatchWalkCapM,   // $8
		config.C.MatchWalkLongM,  // $9
		config.C.MatchMinCarryCityM, // $10
		config.C.MatchCarryK,     // $11
		limit,                    // $12
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
		); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
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

// CreateRideRequest creates a new ride_request row OR returns the existing
// active row's id without mutating it. Idempotent — safe to call from both
// the driver-initiated (SetRoute corridor match) and passenger-initiated
// (RequestDriver) flows. The partial unique index `uniq_active_ride_request`
// (defined in db/postgres.go) scopes uniqueness to status IN ('pending','accepted'),
// so cancelled/declined history rows coexist freely.
//
// Returns (id, isNew, error). isNew=false means the caller should NOT emit a
// fresh ride_request WS notification — the passenger has already been notified
// for this driver.
func CreateRideRequest(driverID, passengerID string, pickupLat, pickupLng float64) (string, bool, error) {
	ctx := context.Background()
	var id string
	err := db.DB.QueryRow(ctx, `
		INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status)
		VALUES ($1, $2, ST_MakePoint($4, $3)::geography, 'pending')
		ON CONFLICT (driver_id, passenger_id) WHERE status IN ('pending','accepted','in_progress') DO NOTHING
		RETURNING id
	`, driverID, passengerID, pickupLat, pickupLng).Scan(&id)
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
		  AND status IN ('pending','accepted','in_progress')
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
		       COALESCE(AVG(r.stars), 0), COUNT(r.id),
		       COALESCE(rr.pickup_code, ''), rr.picked_up_at
		FROM ride_requests rr
		JOIN users u ON u.id = rr.passenger_id
		LEFT JOIN ratings r ON r.rated_id = rr.passenger_id
		WHERE rr.driver_id = $1
		  AND rr.status IN ('pending','accepted','in_progress')
		GROUP BY rr.id, rr.driver_id, rr.passenger_id, rr.pickup_location,
		         rr.status, rr.created_at, u.name, u.photo_url,
		         rr.pickup_code, rr.picked_up_at
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
		); err != nil {
			return nil, err
		}
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

func UpdateRideRequestStatus(requestID, passengerID, status string) (*models.RideRequest, error) {
	var r models.RideRequest
	err := db.DB.QueryRow(context.Background(), `
		UPDATE ride_requests SET status = $1
		WHERE id = $2 AND passenger_id = $3
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

type NearbyDriver struct {
	DriverID   string  `json:"driver_id"`
	DriverName string  `json:"driver_name"`
	OriginLat  float64 `json:"origin_lat"`
	OriginLng  float64 `json:"origin_lng"`
	DestLat    float64 `json:"dest_lat"`
	DestLng    float64 `json:"dest_lng"`
	CorridorKm float64 `json:"corridor_km"`
	Seats      int     `json:"seats"`
	CarMake    string  `json:"car_make"`
	CarModel   string  `json:"car_model"`
	CarColor   string  `json:"car_color"`
	CarPlate   string  `json:"car_plate"`
	RouteWKT   string  `json:"route_wkt"`
}

// GetNearbyDrivers returns active drivers whose origin is within radiusKm of (lat, lng).
// When destRadiusKm > 0, only drivers whose route passes within destRadiusKm of (destLat, destLng) are returned.
// callerID excludes the caller themselves so a user who is both driver and passenger
// (after a role switch with stale state) never sees their own car.
func GetNearbyDrivers(lat, lng, radiusKm, destLat, destLng, destRadiusKm float64, callerID string) ([]NearbyDriver, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT ad.driver_id,
		       u.name,
		       ST_Y(ad.origin::geometry), ST_X(ad.origin::geometry),
		       ST_Y(ad.destination::geometry), ST_X(ad.destination::geometry),
		       ad.corridor_km, ad.seats,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,''),
		       COALESCE(ST_AsText(ad.route), '')
		FROM active_drivers ad
		JOIN users u ON u.id = ad.driver_id
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
	`, lat, lng, radiusKm, destLat, destLng, destRadiusKm, callerID)
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
			&d.CarMake, &d.CarModel, &d.CarColor, &d.CarPlate,
			&d.RouteWKT,
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
