package handlers

import (
	"errors"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// ConfirmPickup is the symmetric pickup-handshake endpoint. Either the
// passenger or the driver may submit the 4-digit code that was generated when
// the ride was accepted; the first valid submission flips the ride_request to
// `in_progress` and emits a `pickup_confirmed` WS event to the other party.
// Subsequent submissions on an already-in-progress row are no-op idempotent
// 200 responses — they don't fire a second WS event.
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

	payload := gin.H{
		"request_id": req.RequestID,
		"by_user_id": userID,
	}
	// Notify the *other* party. The submitter already knows.
	if userID == driverID {
		Hub.SendToUser(passengerID, "pickup_confirmed", payload)
	} else {
		Hub.SendToUser(driverID, "pickup_confirmed", payload)
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
