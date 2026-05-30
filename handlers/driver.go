package handlers

import (
	"context"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
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

	passengers, err := services.FindPassengersInCorridor(route.WKT, req.CorridorKm)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "geo query failed"})
		return
	}

	for _, p := range passengers {
		services.CreateRideRequest(driverID, p.ID, 0, 0)
		services.NotifyPassengerRideAvailable(p.FCMToken, driverID, req.DestinationLat, req.DestinationLng)
		Hub.SendToUser(p.ID, "ride_request", gin.H{
			"driver_id":       driverID,
			"destination_lat": req.DestinationLat,
			"destination_lng": req.DestinationLng,
			"corridor_km":     req.CorridorKm,
			"seats":           req.Seats,
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
					Hub.SendToUser(passengerID, "ride_done", gin.H{
						"request_id": reqID,
						"driver_id":  driverID,
					})
				}
			}
			db.DB.Exec(context.Background(),
				`UPDATE ride_requests SET status = 'cancelled' WHERE driver_id = $1 AND status = 'accepted'`,
				driverID,
			)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func DriverCancelRide(c *gin.Context) {
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")

	var passengerID string
	err := db.DB.QueryRow(context.Background(),
		`UPDATE ride_requests SET status = 'cancelled'
		 WHERE id = $1 AND driver_id = $2 AND status = 'accepted'
		 RETURNING passenger_id`,
		requestID, driverID,
	).Scan(&passengerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ride not found or already cancelled"})
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
	db.DB.Exec(context.Background(),
		`UPDATE ride_requests SET status = 'cancelled' WHERE driver_id = $1 AND status = 'pending'`,
		driverID,
	)
	if err := services.DeleteActiveDriver(driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
