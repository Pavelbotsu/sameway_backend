package services

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
)

func UpsertActiveDriver(driverID, routeWKT string, originLat, originLng, destLat, destLng, corridorKm float64, seats int) error {
	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO active_drivers (driver_id, origin, destination, route, corridor_km, seats, status)
		VALUES (
			$1,
			ST_MakePoint($3, $2)::geography,
			ST_MakePoint($5, $4)::geography,
			ST_GeogFromText($6),
			$7, $8, 'looking'
		)
		ON CONFLICT (driver_id) DO UPDATE SET
			origin      = EXCLUDED.origin,
			destination = EXCLUDED.destination,
			route       = EXCLUDED.route,
			corridor_km = EXCLUDED.corridor_km,
			seats       = EXCLUDED.seats,
			status      = 'looking',
			created_at  = NOW()
	`, driverID, originLat, originLng, destLat, destLng, routeWKT, corridorKm, seats)
	return err
}

func FindPassengersInCorridor(routeWKT string, corridorKm float64) ([]models.User, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT u.id, u.name, u.fcm_token,
		       ST_Y(ap.location::geometry) AS lat,
		       ST_X(ap.location::geometry) AS lng
		FROM active_passengers ap
		JOIN users u ON u.id = ap.passenger_id
		WHERE ST_DWithin(
			ap.location::geography,
			ST_GeogFromText($1),
			$2 * 1000
		)
		AND ap.updated_at > NOW() - INTERVAL '10 minutes'
	`, routeWKT, corridorKm)
	if err != nil {
		return nil, fmt.Errorf("corridor query: %w", err)
	}
	defer rows.Close()

	var passengers []models.User
	for rows.Next() {
		var u models.User
		var lat, lng float64
		if err := rows.Scan(&u.ID, &u.Name, &u.FCMToken, &lat, &lng); err != nil {
			return nil, err
		}
		passengers = append(passengers, u)
	}
	return passengers, rows.Err()
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

func CreateRideRequest(driverID, passengerID string, pickupLat, pickupLng float64) (string, error) {
	var id string
	err := db.DB.QueryRow(context.Background(), `
		INSERT INTO ride_requests (driver_id, passenger_id, pickup_location, status)
		VALUES ($1, $2, ST_MakePoint($4, $3)::geography, 'pending')
		ON CONFLICT (driver_id, passenger_id) DO UPDATE SET
			status     = 'pending',
			created_at = NOW()
		RETURNING id
	`, driverID, passengerID, pickupLat, pickupLng).Scan(&id)
	return id, err
}

func GetDriverRequests(driverID string) ([]models.RideRequest, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT rr.id, rr.driver_id, rr.passenger_id,
		       COALESCE(ST_Y(rr.pickup_location::geometry), 0),
		       COALESCE(ST_X(rr.pickup_location::geometry), 0),
		       rr.status, rr.created_at
		FROM ride_requests rr
		WHERE rr.driver_id = $1
		ORDER BY rr.created_at DESC
	`, driverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []models.RideRequest
	for rows.Next() {
		var r models.RideRequest
		if err := rows.Scan(&r.ID, &r.DriverID, &r.PassengerID, &r.PickupLat, &r.PickupLng, &r.Status, &r.CreatedAt); err != nil {
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
func GetNearbyDrivers(lat, lng, radiusKm, destLat, destLng, destRadiusKm float64) ([]NearbyDriver, error) {
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
	`, lat, lng, radiusKm, destLat, destLng, destRadiusKm)
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
