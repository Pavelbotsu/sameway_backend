package handlers

import (
	"errors"
	"log"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// FlagRideSafety records that a participant answered "no, something is wrong"
// to the mid-ride check-in prompt (see lib/core/ride_check_in.dart, which fires
// when the two parties' fresh GPS fixes diverge while the ride is in_progress).
//
// This is a REVIEW RECORD, not an alert pipeline. Nothing pages anyone. The
// escalation the user actually needs happens client-side and immediately: the
// check-in sheet opens SosSheet right after this call, which offers the local
// emergency number and a location share. Treat the column as something an
// operator reads with psql after the fact.
//
// Design notes:
//   - Auth'd but NOT role-scoped: either party may flag, so this lives on the
//     top-level router next to /ratings and /pickup/confirm rather than in the
//     /driver or /passenger group.
//   - First flag wins (`WHERE safety_flagged_at IS NULL`), so a double-tap or a
//     client retry can't overwrite who raised it or when.
//   - Returns 200 for an already-flagged ride. A safety control must not fail
//     loudly at the exact moment someone is frightened, and "already recorded"
//     is a success from the caller's point of view.
func FlagRideSafety(c *gin.Context) {
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
		Reason    string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	// Participant-of-the-row check, same shape as GetPickupRoute: the ride id
	// is a bearer-ish value that could be guessed or replayed, so membership is
	// verified against the row rather than trusted from the caller.
	var driverID, passengerID string
	err := db.DB.QueryRow(ctx, `
		SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1
	`, req.RequestID).Scan(&driverID, &passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if userID != driverID && userID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not a participant"})
		return
	}

	// Reason is free text from a sheet with fixed options today, but cap it by
	// runes anyway — same reasoning as SubmitBugReport, the app ships Cyrillic
	// locales and a byte-slice truncation would produce invalid UTF-8.
	reason := truncateRunes(req.Reason, 500)

	if _, err := db.DB.Exec(ctx, `
		UPDATE ride_requests
		SET safety_flagged_at = NOW(),
		    safety_flagged_by = $2,
		    safety_flag_reason = NULLIF($3, '')
		WHERE id = $1 AND safety_flagged_at IS NULL
	`, req.RequestID, userID, reason); err != nil {
		// Log loudly — a dropped safety flag is exactly the thing we would want
		// to know about later — but still tell the client it landed, so the UI
		// proceeds straight to the SOS sheet instead of showing an error.
		log.Printf("FlagRideSafety: ride %s by %s: %v", req.RequestID, userID, err)
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
