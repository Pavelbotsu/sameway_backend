package services

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/rideshare-backend/db"
	"github.com/Pavelbotsu/rideshare-backend/models"
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
