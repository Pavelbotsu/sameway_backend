package handlers

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Recovery paths for an accepted ride that can't proceed normally: a locked
// pickup code, a passenger who never shows up, or a ride everyone forgot.
// Before these existed, all three left the ride `accepted` forever, holding
// the driver's seat and blocking the passenger from other matches.

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// releaseSeat gives back the seat an accepted ride held. Same SQL as the
// cancel paths.
func releaseSeat(ctx context.Context, q execer, driverID string) error {
	_, err := q.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = GREATEST(seats_taken - 1, 0),
		    status = CASE WHEN status = 'full' THEN 'looking' ELSE status END
		WHERE driver_id = $1`, driverID)
	return err
}

// RegeneratePickupCode handles POST /driver/pickup-code/:request_id/regenerate:
// a fresh code with the attempt counter reset — the way out of a lockout
// (5 wrong codes). Only the ride's driver, only while it's accepted.
func RegeneratePickupCode(c *gin.Context) {
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")
	code := services.NewPickupCode()

	var passengerID string
	err := db.DB.QueryRow(c.Request.Context(), `
		UPDATE ride_requests SET pickup_code = $3, pickup_attempts = 0
		WHERE id = $1 AND driver_id = $2 AND status = 'accepted'
		RETURNING passenger_id`, requestID, driverID, code,
	).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no accepted ride to reset"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	// The passenger never sees the code (they ask the driver for it), only
	// that they can try again.
	Hub.SendToUser(passengerID, "pickup_code_reset", gin.H{"request_id": requestID})
	go safeGo("match-event", func() { logMatchEvent(requestID, "pickup_code_reset", nil) })()
	c.JSON(http.StatusOK, gin.H{"ok": true, "pickup_code": code})
}

// ReportNoShow handles POST /driver/no-show/:request_id: the driver waited and
// the passenger never came. Allowed once the ride has been accepted for at
// least MatchNoShowWaitS without a confirmed pickup — earlier is answered with
// how long is left. The seat is released and the reason recorded.
func ReportNoShow(c *gin.Context) {
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")
	ctx := c.Request.Context()

	var acceptedFor float64
	err := db.DB.QueryRow(ctx, `
		SELECT EXTRACT(EPOCH FROM NOW() - status_changed_at)
		FROM ride_requests
		WHERE id = $1 AND driver_id = $2 AND status = 'accepted'`,
		requestID, driverID).Scan(&acceptedFor)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no accepted ride for this passenger"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if wait := config.C.MatchNoShowWaitS - acceptedFor; wait > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "wait a little longer before reporting a no-show",
			"code":   "no_show_too_early",
			"wait_s": int(math.Ceil(wait)),
		})
		return
	}

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)
	var passengerID string
	err = tx.QueryRow(ctx, `
		UPDATE ride_requests SET status = 'cancelled', cancel_reason = 'no_show'
		WHERE id = $1 AND driver_id = $2 AND status = 'accepted'
		RETURNING passenger_id`, requestID, driverID).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "ride is no longer accepted"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if err := releaseSeat(ctx, tx, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
		"request_id": requestID, "cancelled_by": "no_show",
	})
	go safeGo("match-event", func() {
		logMatchEvent(requestID, "cancelled", map[string]any{"by": "driver", "reason": "no_show"})
	})()
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ExpireStaleAcceptedOnce cancels accepted rides that were never picked up within
// AcceptedTimeoutMinutes, releasing the seat and telling both sides. Runs from
// the reaper; exported so tests can run one sweep.
func ExpireStaleAcceptedOnce(ctx context.Context) {
	minutes := config.C.AcceptedTimeoutMinutes
	if minutes <= 0 {
		return
	}
	rows, err := db.DB.Query(ctx, `
		WITH expired AS (
			UPDATE ride_requests
			SET status = 'cancelled', cancel_reason = 'expired'
			WHERE status = 'accepted'
			  AND picked_up_at IS NULL
			  AND status_changed_at < NOW() - make_interval(mins => $1)
			RETURNING id, driver_id, passenger_id
		), seats AS (
			UPDATE active_drivers ad
			SET seats_taken = GREATEST(ad.seats_taken - n.cnt, 0),
			    status = CASE WHEN ad.status = 'full' THEN 'looking' ELSE ad.status END
			FROM (SELECT driver_id, COUNT(*) AS cnt FROM expired GROUP BY driver_id) n
			WHERE ad.driver_id = n.driver_id
		)
		SELECT id, driver_id, passenger_id FROM expired`, minutes)
	if err != nil {
		log.Printf("expire stale accepted rides: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var reqID, driverID, passengerID string
		if rows.Scan(&reqID, &driverID, &passengerID) != nil {
			continue
		}
		payload := gin.H{"request_id": reqID, "cancelled_by": "timeout"}
		Hub.SendToUser(driverID, "ride_cancelled", payload)
		Hub.SendToUser(passengerID, "ride_cancelled", payload)
		logMatchEvent(reqID, "expired", map[string]any{"why": "accepted_timeout"})
	}
}
