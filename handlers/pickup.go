package handlers

import (
	"errors"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// ConfirmPickup is the pickup-handshake endpoint. The driver knows the
// 4-digit code (it's stamped on their `ride_response` WS payload and the
// `/driver/requests` JSON); the passenger has to ask the driver verbally
// what it is and type it in here. The asymmetry is the point: a successful
// submission proves the two parties physically met.
//
// Drivers are explicitly forbidden from submitting their own code (D2 in the
// pickup-handshake plan). Subsequent submissions on an already-in-progress
// row return a 200 with `{already: true}` and do not fire a second WS event.
func ConfirmPickup(c *gin.Context) {
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
		Code      string `json:"code" binding:"required,len=4"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var driverID, passengerID, status string
	var pickupCode *string
	err := db.DB.QueryRow(ctx, `
		SELECT driver_id, passenger_id, status, pickup_code
		FROM ride_requests
		WHERE id = $1
	`, req.RequestID).Scan(&driverID, &passengerID, &status, &pickupCode)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// Authorization: caller must be one of the two parties on this ride.
	if userID != driverID && userID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not a participant on this ride"})
		return
	}
	// Asymmetric handshake: only the passenger may submit the code. The
	// driver has it on their screen; they hand it off verbally as proof of
	// physical meeting. See D2 in the matching + pickup-code plan.
	if userID == driverID {
		c.JSON(http.StatusForbidden,
			gin.H{"error": "only the passenger can confirm pickup"})
		return
	}

	// Idempotent: if already in_progress, return 200 with a sentinel so the
	// client can stop showing the confirm UI without panicking.
	if status == "in_progress" {
		c.JSON(http.StatusOK, gin.H{"ok": true, "already": true})
		return
	}

	if status != "accepted" {
		c.JSON(http.StatusConflict, gin.H{"error": "ride is not in the accepted state"})
		return
	}

	if pickupCode == nil || *pickupCode == "" || *pickupCode != req.Code {
		c.JSON(http.StatusBadRequest, gin.H{"error": "wrong code"})
		return
	}

	// Flip status and stamp the pickup time. The partial unique index already
	// covers in_progress so we don't need to worry about competing accepts.
	if _, err := db.DB.Exec(ctx, `
		UPDATE ride_requests
		SET status = 'in_progress', picked_up_at = NOW()
		WHERE id = $1
	`, req.RequestID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// The driver is the "other party" — the submitter is always the passenger.
	Hub.SendToUser(driverID, "pickup_confirmed", gin.H{
		"request_id": req.RequestID,
		"by_user_id": userID,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
