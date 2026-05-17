package handlers

import (
	"context"
	"net/http"

	"github.com/Pavelbotsu/rideshare-backend/db"
	"github.com/Pavelbotsu/rideshare-backend/services"
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

func GoOffline(c *gin.Context) {
	passengerID := c.GetString("user_id")
	if err := services.DeleteActivePassenger(passengerID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
