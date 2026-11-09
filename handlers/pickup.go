package handlers

import (
	"crypto/subtle"
	"errors"
	"log"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// pickupMaxAttempts caps wrong-code submissions per ride. Matches
// resetMaxAttempts in password_reset.go — same shape of control, same budget.
const pickupMaxAttempts = 5

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
	var attempts int
	err := db.DB.QueryRow(ctx, `
		SELECT driver_id, passenger_id, status, pickup_code, pickup_attempts
		FROM ride_requests
		WHERE id = $1
	`, req.RequestID).Scan(&driverID, &passengerID, &status, &pickupCode, &attempts)
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

	// Brute-force cap. The code space is only 10^4, and a successful submission
	// is the sole evidence that the two parties physically met — uncapped, a
	// passenger could script their way to "picked up" without ever meeting the
	// driver, which defeats the entire handshake. Mirrors the attempts cap the
	// reset/verification codes already carry.
	//
	// Once locked, only the driver can unlock it (POST /driver/pickup-code/
	// :id/regenerate issues a new code and resets the counter) — there used to
	// be no way out at all, so a locked ride sat in `accepted` forever.
	if attempts >= pickupMaxAttempts {
		respondPickupLocked(c)
		return
	}
	codeOK := pickupCode != nil && *pickupCode != "" &&
		subtle.ConstantTimeCompare([]byte(*pickupCode), []byte(req.Code)) == 1
	if !codeOK {
		// Atomic check-and-increment: parallel guesses can't each pass a stale
		// "attempts < max" read and exceed the cap.
		var now int
		err := db.DB.QueryRow(ctx, `
			UPDATE ride_requests SET pickup_attempts = pickup_attempts + 1
			WHERE id = $1 AND status = 'accepted' AND pickup_attempts < $2
			RETURNING pickup_attempts`, req.RequestID, pickupMaxAttempts,
		).Scan(&now)
		if errors.Is(err, pgx.ErrNoRows) {
			respondPickupLocked(c)
			return
		}
		if err != nil {
			log.Printf("ConfirmPickup: bump attempts for %s: %v", req.RequestID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if now >= pickupMaxAttempts {
			// Tell the driver now, so their screen offers a new code before
			// the passenger even asks.
			Hub.SendToUser(driverID, "pickup_locked", gin.H{"request_id": req.RequestID})
			respondPickupLocked(c)
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "wrong code", "code": "wrong_code",
			"attempts_left": pickupMaxAttempts - now,
		})
		return
	}

	// Flip status and stamp the pickup time. Guarded on the lock too, so a
	// lockout reached by a parallel request between the read above and here
	// still holds.
	tag, err := db.DB.Exec(ctx, `
		UPDATE ride_requests
		SET status = 'in_progress', picked_up_at = NOW()
		WHERE id = $1 AND status = 'accepted' AND pickup_attempts < $2
	`, req.RequestID, pickupMaxAttempts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if tag.RowsAffected() == 0 {
		respondPickupLocked(c)
		return
	}

	// The driver is the "other party" — the submitter is always the passenger.
	Hub.SendToUser(driverID, "pickup_confirmed", gin.H{
		"request_id": req.RequestID,
		"by_user_id": userID,
	})

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func respondPickupLocked(c *gin.Context) {
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error":         "too many incorrect codes — ask the driver for a new code",
		"code":          "pickup_locked",
		"attempts_left": 0,
	})
}
