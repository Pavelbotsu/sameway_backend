package services

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
)

func CreatePlannedTrip(driverID, originName string, originLat, originLng float64,
	destName string, destLat, destLng, corridorKm, distanceKm float64,
	seats int, plannedAt string, routeWKT string) (string, error) {

	var id string
	err := db.DB.QueryRow(context.Background(), `
		INSERT INTO planned_trips
			(driver_id, origin_name, origin, destination_name, destination, route, seats, corridor_km, distance_km, planned_at)
		VALUES (
			$1, $2, ST_MakePoint($4, $3)::geography, $5, ST_MakePoint($7, $6)::geography,
			CASE WHEN $8 = '' THEN NULL ELSE ST_GeogFromText($8) END,
			$9, $10, $11, $12::timestamptz
		)
		RETURNING id
	`, driverID, originName, originLat, originLng, destName, destLat, destLng,
		routeWKT, seats, corridorKm, distanceKm, plannedAt).Scan(&id)
	return id, err
}

func GetDriverPlannedTrips(driverID string) ([]models.PlannedTrip, error) {
	rows, err := db.DB.Query(context.Background(), `
		SELECT pt.id, pt.driver_id, u.name,
		       COALESCE(u.car_color||' '||u.car_make||' '||u.car_model, ''),
		       pt.origin_name, ST_Y(pt.origin::geometry), ST_X(pt.origin::geometry),
		       pt.destination_name, ST_Y(pt.destination::geometry), ST_X(pt.destination::geometry),
		       pt.seats, pt.corridor_km, pt.distance_km, pt.planned_at, pt.status, pt.created_at
		FROM planned_trips pt
		JOIN users u ON u.id = pt.driver_id
		WHERE pt.driver_id = $1
		ORDER BY pt.planned_at ASC
	`, driverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlannedTrips(rows)
}

func SearchPlannedTrips(originLat, originLng, destLat, destLng, radiusKm float64, sortBy, plannedAfter string) ([]models.PlannedTrip, error) {
	orderClause := "pt.planned_at ASC"
	switch sortBy {
	case "distance":
		orderClause = "ST_Distance(pt.origin::geography, ST_MakePoint($2,$1)::geography) ASC"
	case "destination":
		orderClause = "pt.destination_name ASC"
	}

	destFilter := ""
	if destLat != 0 && destLng != 0 {
		destFilter = fmt.Sprintf(`AND ST_DWithin(pt.destination::geography, ST_MakePoint(%f,%f)::geography, %f * 1000)`,
			destLng, destLat, radiusKm)
	}

	afterClause := "NOW()"
	if plannedAfter != "" {
		afterClause = fmt.Sprintf("'%s'::timestamptz", plannedAfter)
	}

	query := fmt.Sprintf(`
		SELECT pt.id, pt.driver_id, u.name,
		       COALESCE(u.car_color||' '||u.car_make||' '||u.car_model, ''),
		       pt.origin_name, ST_Y(pt.origin::geometry), ST_X(pt.origin::geometry),
		       pt.destination_name, ST_Y(pt.destination::geometry), ST_X(pt.destination::geometry),
		       pt.seats, pt.corridor_km, pt.distance_km, pt.planned_at, pt.status, pt.created_at
		FROM planned_trips pt
		JOIN users u ON u.id = pt.driver_id
		WHERE ST_DWithin(pt.origin::geography, ST_MakePoint($2,$1)::geography, $3 * 1000)
		%s
		AND pt.planned_at > %s
		AND pt.status = 'open'
		ORDER BY %s
		LIMIT 50
	`, destFilter, afterClause, orderClause)

	rows, err := db.DB.Query(context.Background(), query, originLat, originLng, radiusKm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlannedTrips(rows)
}

func DeletePlannedTrip(tripID, driverID string) error {
	_, err := db.DB.Exec(context.Background(),
		`UPDATE planned_trips SET status = 'cancelled' WHERE id = $1 AND driver_id = $2`,
		tripID, driverID)
	return err
}

func scanPlannedTrips(rows interface{ Next() bool; Scan(...any) error }) ([]models.PlannedTrip, error) {
	var trips []models.PlannedTrip
	for rows.Next() {
		var t models.PlannedTrip
		if err := rows.Scan(
			&t.ID, &t.DriverID, &t.DriverName, &t.CarSummary,
			&t.OriginName, &t.OriginLat, &t.OriginLng,
			&t.DestinationName, &t.DestLat, &t.DestLng,
			&t.Seats, &t.CorridorKm, &t.DistanceKm, &t.PlannedAt, &t.Status, &t.CreatedAt,
		); err != nil {
			return nil, err
		}
		trips = append(trips, t)
	}
	return trips, nil
}
