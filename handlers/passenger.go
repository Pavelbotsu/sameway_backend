package handlers

import (
	"context"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
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
	rideReq, err := services.UpdateRideRequestStatus(req.RequestID, passengerID, req.Status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	var driverFCM string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`, rideReq.DriverID,
	).Scan(&driverFCM)

	services.NotifyDriverPassengerResponded(driverFCM, passengerID, req.Status)
	Hub.SendToUser(rideReq.DriverID, "ride_response", gin.H{
		"passenger_id": passengerID,
		"request_id":   req.RequestID,
		"status":       req.Status,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
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

	Hub.SendToUser(req.DriverID, "passenger_request", gin.H{
		"request_id":   requestID,
		"passenger_id": passengerID,
		"pickup_lat":   pickupLat,
		"pickup_lng":   pickupLng,
	})

	var driverFCM string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token,'') FROM users WHERE id = $1`, req.DriverID,
	).Scan(&driverFCM)
	services.NotifyDriverPassengerRequested(driverFCM, passengerID)

	c.JSON(http.StatusOK, gin.H{"ok": true, "request_id": requestID})
}

func GoOffline(c *gin.Context) {
	passengerID := c.GetString("user_id")
	if err := services.DeleteActivePassenger(passengerID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
