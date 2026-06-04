package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func SetRoute(c *gin.Context) {
	var req struct {
		OriginLat      float64 `json:"origin_lat" binding:"required"`
		OriginLng      float64 `json:"origin_lng" binding:"required"`
		DestinationLat float64 `json:"destination_lat" binding:"required"`
		DestinationLng float64 `json:"destination_lng" binding:"required"`
		CorridorKm     float64 `json:"corridor_km" binding:"required"`
		Seats          int     `json:"seats" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	driverID := c.GetString("user_id")

	route, err := services.GetRoadRoute(req.OriginLat, req.OriginLng, req.DestinationLat, req.DestinationLng)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "routing failed: " + err.Error()})
		return
	}

	if err := services.UpsertActiveDriver(driverID, route.WKT,
		req.OriginLat, req.OriginLng, req.DestinationLat, req.DestinationLng,
		req.CorridorKm, req.Seats); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	passengers, err := services.FindPassengersInCorridor(route.WKT, req.CorridorKm, config.C.MaxCorridorNotifications)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "geo query failed"})
		return
	}
	log.Printf("SetRoute driver=%s matched %d passengers in corridor (corridor_km=%.2f limit=%d)",
		driverID, len(passengers), req.CorridorKm, config.C.MaxCorridorNotifications)

	var (
		driverName, carMake, carModel, carColor, carPlate string
		driverAvgRating                                   float64
		driverRatingCount                                 int
	)
	db.DB.QueryRow(context.Background(), `
		SELECT u.name,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,''),
		       COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name, u.car_make, u.car_model, u.car_color, u.car_plate
	`, driverID).Scan(&driverName, &carMake, &carModel, &carColor, &carPlate, &driverAvgRating, &driverRatingCount)

	for _, p := range passengers {
		requestID, _ := services.CreateRideRequest(driverID, p.ID, 0, 0)
		services.NotifyPassengerRideAvailable(p.FCMToken, driverID, req.DestinationLat, req.DestinationLng)
		Hub.SendToUser(p.ID, "ride_request", gin.H{
			"request_id":          requestID,
			"driver_id":           driverID,
			"driver_name":         driverName,
			"driver_avg_rating":   driverAvgRating,
			"driver_rating_count": driverRatingCount,
			"car_make":            carMake,
			"car_model":           carModel,
			"car_color":           carColor,
			"car_plate":           carPlate,
			"destination_lat":     req.DestinationLat,
			"destination_lng":     req.DestinationLng,
			"corridor_km":         req.CorridorKm,
			"seats":               req.Seats,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"route_wkt":      route.WKT,
		"distance_km":    route.DistanceKm,
		"notified_count": len(passengers),
	})
}

func UpdateLocation(c *gin.Context) {
	var req struct {
		Lat float64 `json:"lat" binding:"required"`
		Lng float64 `json:"lng" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	driverID := c.GetString("user_id")
	if db.Redis != nil {
		db.Redis.HSet(context.Background(), "driver:location:"+driverID, "lat", req.Lat, "lng", req.Lng)
	}

	// Relay to any accepted passenger(s) over WS so the passenger's
	// _AcceptedCard mini-map sees the live driver position.
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT id, passenger_id FROM ride_requests
		WHERE driver_id = $1 AND status = 'accepted'
	`, driverID)
	if err == nil {
		for rows.Next() {
			var reqID, passengerID string
			if rows.Scan(&reqID, &passengerID) == nil {
				Hub.SendToUser(passengerID, "driver_location", gin.H{
					"request_id": reqID,
					"lat":        req.Lat,
					"lng":        req.Lng,
				})
			}
		}
		rows.Close()
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func GetRequests(c *gin.Context) {
	driverID := c.GetString("user_id")
	requests, err := services.GetDriverRequests(driverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if requests == nil {
		requests = []models.RideRequest{}
	}
	c.JSON(http.StatusOK, requests)
}

func UpdateStatus(c *gin.Context) {
	var req struct {
		Status     string  `json:"status" binding:"required,oneof=looking full in_progress done"`
		DistanceKm float64 `json:"distance_km"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	driverID := c.GetString("user_id")
	if err := services.UpdateDriverStatus(driverID, req.Status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if req.Status == "done" {
		rows, err := db.DB.Query(context.Background(), `
			SELECT id, passenger_id FROM ride_requests
			WHERE driver_id = $1 AND status = 'accepted'
		`, driverID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var reqID, passengerID string
				if rows.Scan(&reqID, &passengerID) == nil {
					if req.DistanceKm > 0 {
						services.RecordCompletedTrip(reqID, driverID, passengerID, req.DistanceKm)
					}
					payload := gin.H{
						"request_id":   reqID,
						"driver_id":    driverID,
						"passenger_id": passengerID,
					}
					Hub.SendToUser(passengerID, "ride_done", payload)
					Hub.SendToUser(driverID, "ride_done", payload)
				}
			}
			db.DB.Exec(context.Background(),
				`UPDATE ride_requests SET status = 'cancelled' WHERE driver_id = $1 AND status = 'accepted'`,
				driverID,
			)
			db.DB.Exec(context.Background(),
				`UPDATE active_drivers SET seats_taken = 0 WHERE driver_id = $1`,
				driverID,
			)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func DriverCancelRide(c *gin.Context) {
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	var passengerID string
	err = tx.QueryRow(ctx,
		`UPDATE ride_requests SET status = 'cancelled'
		 WHERE id = $1 AND driver_id = $2 AND status = 'accepted'
		 RETURNING passenger_id`,
		requestID, driverID,
	).Scan(&passengerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ride not found or already cancelled"})
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = GREATEST(seats_taken - 1, 0),
		    status = CASE WHEN status = 'full' THEN 'looking' ELSE status END
		WHERE driver_id = $1
	`, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
		"request_id":   requestID,
		"cancelled_by": "driver",
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func DeleteRoute(c *gin.Context) {
	driverID := c.GetString("user_id")

	rows, err := db.DB.Query(context.Background(),
		`UPDATE ride_requests
		 SET status = 'cancelled'
		 WHERE driver_id = $1 AND status IN ('pending','accepted')
		 RETURNING id, passenger_id`,
		driverID,
	)
	if err == nil {
		for rows.Next() {
			var reqID, passengerID string
			if rows.Scan(&reqID, &passengerID) == nil {
				Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
					"request_id":   reqID,
					"cancelled_by": "driver",
				})
			}
		}
		rows.Close()
	}

	if err := services.DeleteActiveDriver(driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DriverRespondToRide mirrors RespondToRide on the passenger side. When a
// passenger has sent a request via RequestDriver, the driver uses this
// endpoint to accept or decline. Accept path runs inside a pgx transaction
// matching respondAccept (seat reservation + auto-cancel competing rows).
func DriverRespondToRide(c *gin.Context) {
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
		Status    string `json:"status" binding:"required,oneof=accepted declined"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	driverID := c.GetString("user_id")

	if req.Status == "declined" {
		driverRespondDecline(c, driverID, req.RequestID)
		return
	}
	driverRespondAccept(c, driverID, req.RequestID)
}

func driverRespondDecline(c *gin.Context, driverID, requestID string) {
	var passengerID string
	err := db.DB.QueryRow(c.Request.Context(), `
		UPDATE ride_requests SET status = 'declined'
		WHERE id = $1 AND driver_id = $2 AND status = 'pending'
		RETURNING passenger_id
	`, requestID, driverID).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request not pending"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	name, avg, count, carMake, carModel, carColor, carPlate := loadDriverProfile(driverID)
	Hub.SendToUser(passengerID, "driver_response", gin.H{
		"request_id":          requestID,
		"driver_id":           driverID,
		"driver_name":         name,
		"driver_avg_rating":   avg,
		"driver_rating_count": count,
		"car_make":            carMake,
		"car_model":           carModel,
		"car_color":           carColor,
		"car_plate":           carPlate,
		"status":              "declined",
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func driverRespondAccept(c *gin.Context, driverID, requestID string) {
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	// 1) Lock the pending request and identify the passenger.
	var passengerID string
	err = tx.QueryRow(ctx, `
		SELECT passenger_id FROM ride_requests
		WHERE id = $1 AND driver_id = $2 AND status = 'pending'
		FOR UPDATE
	`, requestID, driverID).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request not pending"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 2) Lock the driver's own capacity row, validate & reserve a seat.
	var seats, taken int
	err = tx.QueryRow(ctx, `
		SELECT seats, seats_taken FROM active_drivers
		WHERE driver_id = $1 AND status IN ('looking','full')
		FOR UPDATE
	`, driverID).Scan(&seats, &taken)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusGone, gin.H{"error": "no active route"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if taken >= seats {
		c.JSON(http.StatusConflict, gin.H{"error": "ride is full"})
		return
	}
	newTaken := taken + 1
	newStatus := "looking"
	if newTaken >= seats {
		newStatus = "full"
	}
	if _, err = tx.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = $1, status = $2
		WHERE driver_id = $3
	`, newTaken, newStatus, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 3) Flip THIS request to accepted.
	if _, err = tx.Exec(ctx, `
		UPDATE ride_requests SET status = 'accepted' WHERE id = $1
	`, requestID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 4) Auto-cancel the PASSENGER's other pending/accepted rows.
	rows, err := tx.Query(ctx, `
		UPDATE ride_requests SET status = 'cancelled'
		WHERE passenger_id = $1
		  AND status IN ('pending','accepted')
		  AND id <> $2
		RETURNING id, driver_id
	`, passengerID, requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	type bumped struct{ ID, DriverID string }
	var bumpedRows []bumped
	for rows.Next() {
		var b bumped
		if rows.Scan(&b.ID, &b.DriverID) == nil {
			bumpedRows = append(bumpedRows, b)
		}
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 5) Post-commit WS notifications.
	name, avg, count, carMake, carModel, carColor, carPlate := loadDriverProfile(driverID)
	Hub.SendToUser(passengerID, "driver_response", gin.H{
		"request_id":          requestID,
		"driver_id":           driverID,
		"driver_name":         name,
		"driver_avg_rating":   avg,
		"driver_rating_count": count,
		"car_make":            carMake,
		"car_model":           carModel,
		"car_color":           carColor,
		"car_plate":           carPlate,
		"status":              "accepted",
	})
	for _, b := range bumpedRows {
		Hub.SendToUser(b.DriverID, "ride_cancelled", gin.H{
			"request_id":   b.ID,
			"cancelled_by": "passenger_matched_elsewhere",
		})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func loadDriverProfile(driverID string) (name string, avgRating float64, ratingCount int,
	carMake, carModel, carColor, carPlate string) {
	db.DB.QueryRow(context.Background(), `
		SELECT u.name,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,''),
		       COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name, u.car_make, u.car_model, u.car_color, u.car_plate
	`, driverID).Scan(&name, &carMake, &carModel, &carColor, &carPlate, &avgRating, &ratingCount)
	return
}
