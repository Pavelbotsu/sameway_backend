package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Passenger struct {
	ID       string
	Name     string
	FCMToken string
}

// FindPassengersNearRoute looks for passengers within the specified distance of the driver's path
func FindPassengersNearRoute(db *pgxpool.Pool, routeWKT string, bufferMeters float64) ([]Passenger, error) {
	query := `
		SELECT u.id, u.name, u.fcm_token
		FROM active_passengers ap
		JOIN users u ON u.id = ap.passenger_id
		WHERE ST_DWithin(
			ap.current_location, 
			ST_GeogFromText($1), 
			$2
		);
	`

	rows, err := db.Query(context.Background(), query, routeWKT, bufferMeters)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var passengers []Passenger
	for rows.Next() {
		var p Passenger
		if err := rows.Scan(&p.ID, &p.Name, &p.FCMToken); err != nil {
			return nil, err
		}
		passengers = append(passengers, p)
	}
	return passengers, nil
}
