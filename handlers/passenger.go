package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

func PassengerUpdateLocation(c *gin.Context) {
	var req struct {
		Lat float64 `json:"lat" binding:"required"`
		Lng float64 `json:"lng" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	passengerID := c.GetString("user_id")
	if err := services.UpsertActivePassenger(passengerID, req.Lat, req.Lng); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// Relay to the matched driver(s) over WS so the driver UI can
	// render the passenger's live position on the accepted-row mini-map.
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT id, driver_id FROM ride_requests
		WHERE passenger_id = $1 AND status = 'accepted'
	`, passengerID)
	if err == nil {
		for rows.Next() {
			var reqID, driverID string
			if rows.Scan(&reqID, &driverID) == nil {
				Hub.SendToUser(driverID, "passenger_location", gin.H{
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

func RespondToRide(c *gin.Context) {
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
		Status    string `json:"status" binding:"required,oneof=accepted declined"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	passengerID := c.GetString("user_id")

	if req.Status == "declined" {
		respondDecline(c, passengerID, req.RequestID)
		return
	}
	respondAccept(c, passengerID, req.RequestID)
}

func respondDecline(c *gin.Context, passengerID, requestID string) {
	rideReq, err := services.UpdateRideRequestStatus(requestID, passengerID, "declined")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	var driverFCM string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`, rideReq.DriverID,
	).Scan(&driverFCM)

	passengerName, avg, count := loadPassengerProfile(passengerID)

	services.NotifyDriverPassengerResponded(driverFCM, passengerID, "declined")
	Hub.SendToUser(rideReq.DriverID, "ride_response", gin.H{
		"passenger_id":           passengerID,
		"passenger_name":         passengerName,
		"passenger_avg_rating":   avg,
		"passenger_rating_count": count,
		"request_id":             requestID,
		"status":                 "declined",
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type bumpedRequest struct {
	ID       string
	DriverID string
}

func respondAccept(c *gin.Context, passengerID, requestID string) {
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	// 1) Lock the matching pending ride_request and find its driver.
	var driverID string
	err = tx.QueryRow(ctx, `
		SELECT driver_id FROM ride_requests
		WHERE id = $1 AND passenger_id = $2 AND status = 'pending'
		FOR UPDATE
	`, requestID, passengerID).Scan(&driverID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request not pending"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 2) Lock the driver's capacity row, validate & reserve a seat.
	var seats, taken int
	err = tx.QueryRow(ctx, `
		SELECT seats, seats_taken FROM active_drivers
		WHERE driver_id = $1 AND status IN ('looking','full')
		FOR UPDATE
	`, driverID).Scan(&seats, &taken)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusGone, gin.H{"error": "driver no longer active"})
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

	// 3) Flip this request to accepted.
	if _, err = tx.Exec(ctx, `
		UPDATE ride_requests SET status = 'accepted'
		WHERE id = $1
	`, requestID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 4) Auto-cancel passenger's OTHER pending/accepted rows.
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
	var bumped []bumpedRequest
	for rows.Next() {
		var b bumpedRequest
		if rows.Scan(&b.ID, &b.DriverID) == nil {
			bumped = append(bumped, b)
		}
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 5) Post-commit notifications (best-effort).
	var driverFCM string
	db.DB.QueryRow(ctx,
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`, driverID,
	).Scan(&driverFCM)

	passengerName, avg, count := loadPassengerProfile(passengerID)

	services.NotifyDriverPassengerResponded(driverFCM, passengerID, "accepted")
	Hub.SendToUser(driverID, "ride_response", gin.H{
		"passenger_id":           passengerID,
		"passenger_name":         passengerName,
		"passenger_avg_rating":   avg,
		"passenger_rating_count": count,
		"request_id":             requestID,
		"status":                 "accepted",
	})

	for _, b := range bumped {
		Hub.SendToUser(b.DriverID, "ride_cancelled", gin.H{
			"request_id":   b.ID,
			"cancelled_by": "passenger_matched_elsewhere",
		})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func loadPassengerProfile(passengerID string) (name string, avgRating float64, ratingCount int) {
	db.DB.QueryRow(context.Background(), `
		SELECT u.name, COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name
	`, passengerID).Scan(&name, &avgRating, &ratingCount)
	return
}

// GetMyRequests returns this passenger's pending and accepted ride_requests
// with the driver's profile + car info attached, so the passenger UI can show
// an outstanding-requests list with status badges.
func GetMyRequests(c *gin.Context) {
	passengerID := c.GetString("user_id")
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT rr.id, rr.driver_id, rr.status, rr.created_at,
		       u.name,
		       COALESCE(AVG(r.stars), 0), COUNT(r.id),
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,'')
		FROM ride_requests rr
		JOIN users u ON u.id = rr.driver_id
		LEFT JOIN ratings r ON r.rated_id = rr.driver_id
		WHERE rr.passenger_id = $1
		  AND rr.status IN ('pending','accepted')
		GROUP BY rr.id, rr.driver_id, rr.status, rr.created_at,
		         u.name, u.car_make, u.car_model, u.car_color, u.car_plate
		ORDER BY rr.created_at DESC
	`, passengerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	out := make([]gin.H, 0)
	for rows.Next() {
		var id, driverID, status, name string
		var carMake, carModel, carColor, carPlate string
		var createdAt time.Time
		var avg float64
		var count int
		if err := rows.Scan(&id, &driverID, &status, &createdAt, &name,
			&avg, &count, &carMake, &carModel, &carColor, &carPlate); err != nil {
			continue
		}
		out = append(out, gin.H{
			"request_id":          id,
			"driver_id":           driverID,
			"status":              status,
			"created_at":          createdAt.Format(time.RFC3339),
			"driver_name":         name,
			"driver_avg_rating":   avg,
			"driver_rating_count": count,
			"car_make":            carMake,
			"car_model":           carModel,
			"car_color":           carColor,
			"car_plate":           carPlate,
		})
	}
	c.JSON(http.StatusOK, out)
}

func NearbyDrivers(c *gin.Context) {
	var req struct {
		Lat          float64 `json:"latitude" binding:"required"`
		Lng          float64 `json:"longitude" binding:"required"`
		RadiusKm     float64 `json:"radius_km"`
		DestLat      float64 `json:"dest_lat"`
		DestLng      float64 `json:"dest_lng"`
		DestRadiusKm float64 `json:"dest_radius_km"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RadiusKm <= 0 {
		req.RadiusKm = 10
	}
	drivers, err := services.GetNearbyDrivers(req.Lat, req.Lng, req.RadiusKm, req.DestLat, req.DestLng, req.DestRadiusKm)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if drivers == nil {
		drivers = []services.NearbyDriver{}
	}
	c.JSON(http.StatusOK, drivers)
}

func RequestDriver(c *gin.Context) {
	var req struct {
		DriverID string `json:"driver_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	passengerID := c.GetString("user_id")

	var pickupLat, pickupLng float64
	err := db.DB.QueryRow(context.Background(), `
		SELECT ST_Y(location::geometry), ST_X(location::geometry)
		FROM active_passengers WHERE passenger_id = $1`, passengerID,
	).Scan(&pickupLat, &pickupLng)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "update your location first"})
		return
	}

	requestID, err := services.CreateRideRequest(req.DriverID, passengerID, pickupLat, pickupLng)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	var passengerName string
	var passengerAvgRating float64
	var passengerRatingCount int
	db.DB.QueryRow(context.Background(), `
		SELECT u.name, COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name
	`, passengerID).Scan(&passengerName, &passengerAvgRating, &passengerRatingCount)

	Hub.SendToUser(req.DriverID, "passenger_request", gin.H{
		"request_id":             requestID,
		"passenger_id":           passengerID,
		"passenger_name":         passengerName,
		"passenger_avg_rating":   passengerAvgRating,
		"passenger_rating_count": passengerRatingCount,
		"pickup_lat":             pickupLat,
		"pickup_lng":             pickupLng,
	})

	var driverFCM string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token,'') FROM users WHERE id = $1`, req.DriverID,
	).Scan(&driverFCM)
	services.NotifyDriverPassengerRequested(driverFCM, passengerID)

	c.JSON(http.StatusOK, gin.H{"ok": true, "request_id": requestID})
}

func PassengerCancelRide(c *gin.Context) {
	requestID := c.Param("request_id")
	passengerID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	var driverID string
	err = tx.QueryRow(ctx,
		`UPDATE ride_requests SET status = 'cancelled'
		 WHERE id = $1 AND passenger_id = $2 AND status = 'accepted'
		 RETURNING driver_id`,
		requestID, passengerID,
	).Scan(&driverID)
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

	Hub.SendToUser(driverID, "ride_cancelled", gin.H{
		"request_id":   requestID,
		"cancelled_by": "passenger",
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// PassengerCancelPendingRequest cancels a passenger's outstanding pending
// ride_request (one that hasn't been accepted yet). Distinct from
// PassengerCancelRide which only acts on accepted rows and adjusts driver
// capacity. Emits passenger_request_cancelled to the driver so their pending
// tile disappears.
func PassengerCancelPendingRequest(c *gin.Context) {
	requestID := c.Param("request_id")
	passengerID := c.GetString("user_id")

	var driverID string
	err := db.DB.QueryRow(c.Request.Context(),
		`UPDATE ride_requests SET status = 'cancelled'
		 WHERE id = $1 AND passenger_id = $2 AND status = 'pending'
		 RETURNING driver_id`,
		requestID, passengerID,
	).Scan(&driverID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request not found or no longer pending"})
		return
	}

	Hub.SendToUser(driverID, "passenger_request_cancelled", gin.H{
		"request_id":   requestID,
		"cancelled_by": "passenger",
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func GoOffline(c *gin.Context) {
	passengerID := c.GetString("user_id")
	if err := services.DeleteActivePassenger(passengerID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
