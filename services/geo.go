package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/jackc/pgx/v5"
)

// destinationMatchMeters bounds how far a passenger's destination may be from
// the driver's destination for the driver-initiated match to fire. NULL on
// active_passengers.destination_lat means the passenger has not declared a
// destination yet — they fall back to the legacy "any corridor match" behavior.
const destinationMatchMeters = 1500

// Ride-sharing tunables. earlyNotifyRadiusM caps how far ahead of the driver
// (measured along the route) a passenger may be and still get notified. The
// goal is "driver is approaching" notice — not "anywhere in the corridor".
// maxWalkM caps how far the passenger is willing to walk to the pickup point
// on the road; if it's further than this the match is dropped. The ahead
// tolerance keeps a driver standing exactly at the projection point from
// fluttering between matched/unmatched.
const (
	earlyNotifyRadiusM     = 2000
	maxWalkM               = 1000
	aheadToleranceFraction = 0.01
)

// CorridorMatch is the result row of the heading-aware corridor query: the
// matched passenger, the suggested pickup point on the driver's road, and the
// two distances the UI needs to render walk/drive ETAs.
type CorridorMatch struct {
	User            models.User
	PickupLat       float64
	PickupLng       float64
	WalkM           float64
	DriverToPickupM float64
}

func UpsertActiveDriver(driverID, routeWKT string, originLat, originLng, destLat, destLng, corridorKm float64, seats int) error {
	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO active_drivers (driver_id, origin, destination, route, current_location, corridor_km, seats, status)
		VALUES (
			$1,
			ST_MakePoint($3, $2)::geography,
			ST_MakePoint($5, $4)::geography,
			ST_GeogFromText($6),
			ST_MakePoint($3, $2)::geography,
			$7, $8, 'looking'
		)
		ON CONFLICT (driver_id) DO UPDATE SET
			origin           = EXCLUDED.origin,
			destination      = EXCLUDED.destination,
			route            = EXCLUDED.route,
			current_location = EXCLUDED.origin,
			corridor_km      = EXCLUDED.corridor_km,
			seats            = EXCLUDED.seats,
			seats_taken      = 0,
			status           = 'looking',
			created_at       = NOW()
	`, driverID, originLat, originLng, destLat, destLng, routeWKT, corridorKm, seats)
	return err
}

// FindPassengersInCorridor matches passengers who are (a) inside the driver's
// corridor, (b) AHEAD of the driver's current position along the route, (c)
// within earlyNotifyRadiusM ahead, and (d) within maxWalkM of the road. For
// each match it computes the suggested pickup point on the road
// (ST_ClosestPoint), the passenger's walking distance to that point, and the
// driver's along-route distance to that point.
func FindPassengersInCorridor(driverID, routeWKT string, corridorKm, destLat, destLng float64, limit int) ([]CorridorMatch, error) {
	rows, err := db.DB.Query(context.Background(), `
		WITH route_info AS (
			SELECT ad.route::geometry AS route_geom,
			       ST_Length(ad.route) AS route_len_m,
			       ST_LineLocatePoint(
			           ad.route::geometry,
			           COALESCE(ad.current_location, ad.origin)::geometry
			       ) AS driver_frac
			FROM active_drivers ad
			WHERE ad.driver_id = $1
		),
		candidates AS (
			SELECT u.id, u.name, COALESCE(u.fcm_token, '') AS fcm_token,
			       ap.location AS pax_loc,
			       ST_LineLocatePoint(ri.route_geom, ap.location::geometry) AS pax_frac,
			       ST_ClosestPoint(ri.route_geom, ap.location::geometry)::geography AS pickup_geog,
			       ri.route_len_m, ri.driver_frac
			FROM active_passengers ap
			JOIN users u ON u.id = ap.passenger_id
			CROSS JOIN route_info ri
			WHERE ST_DWithin(
				ap.location::geography,
				ST_GeogFromText($2),
				$3 * 1000
			)
			AND ap.updated_at > NOW() - INTERVAL '10 minutes'
			AND NOT EXISTS (
				SELECT 1 FROM ride_requests rr
				WHERE rr.passenger_id = u.id
				  AND rr.status IN ('pending','accepted')
			)
			AND (
				ap.destination_lat IS NULL
				OR ST_DWithin(
					ST_SetSRID(ST_MakePoint(ap.destination_lng, ap.destination_lat), 4326)::geography,
					ST_SetSRID(ST_MakePoint($5, $4), 4326)::geography,
					$6
				)
			)
		)
		SELECT id, name, fcm_token,
		       ST_Y(pickup_geog::geometry), ST_X(pickup_geog::geometry),
		       ST_Distance(pax_loc, pickup_geog) AS walk_m,
		       (pax_frac - driver_frac) * route_len_m AS driver_to_pickup_m
		FROM candidates
		WHERE pax_frac > driver_frac + $7
		  AND ST_Distance(pax_loc, pickup_geog) < $8
		  AND (pax_frac - driver_frac) * route_len_m < $9
		ORDER BY driver_to_pickup_m ASC
		LIMIT $10
	`,
		driverID, routeWKT, corridorKm,
		destLat, destLng, destinationMatchMeters,
		aheadToleranceFraction, float64(maxWalkM), float64(earlyNotifyRadiusM),
		limit,
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
			&m.WalkM, &m.DriverToPickupM,
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
		       u.name,
		       COALESCE(AVG(r.stars), 0), COUNT(r.id),
		       COALESCE(rr.pickup_code, ''), rr.picked_up_at
		FROM ride_requests rr
		JOIN users u ON u.id = rr.passenger_id
		LEFT JOIN ratings r ON r.rated_id = rr.passenger_id
		WHERE rr.driver_id = $1
		  AND rr.status IN ('pending','accepted','in_progress')
		GROUP BY rr.id, rr.driver_id, rr.passenger_id, rr.pickup_location,
		         rr.status, rr.created_at, u.name, rr.pickup_code, rr.picked_up_at
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
			&r.PassengerName,
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
