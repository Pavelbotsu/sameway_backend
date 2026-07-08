package handlers

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
)

// ReapPendingRideRequests runs forever, sweeping the ride_requests table for
// rows that have been pending past the configured timeout and cancelling them.
// Both sides receive a ride_cancelled WS event with cancelled_by="timeout".
func ReapPendingRideRequests(timeoutMinutes int) {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 5
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Each sweep is panic-isolated: one malformed row must not take the
		// whole server (and every WS connection) down with it.
		safeGo("reaper-sweep", func() {
			ReapStalePendingOnce(timeoutMinutes)
			sweepLiveLocThrottle()
			sweepCorridorRerunThrottle()
		})()
	}
}

// ReapStalePendingOnce performs a single sweep. Exported so tests can invoke
// it synchronously without depending on the ticker.
func ReapStalePendingOnce(timeoutMinutes int) {
	// Sweep both un-answered driver offers ('offered') and un-answered
	// driver-approval requests ('pending') that have aged past the timeout.
	rows, err := db.DB.Query(context.Background(), `
		UPDATE ride_requests
		SET status = 'cancelled'
		WHERE status IN ('offered','pending')
		  AND created_at < NOW() - ($1 || ' minutes')::interval
		RETURNING id, driver_id, passenger_id
	`, strconv.Itoa(timeoutMinutes))
	if err != nil {
		log.Printf("reaper query: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var reqID, dID, pID string
		if rows.Scan(&reqID, &dID, &pID) != nil {
			continue
		}
		payload := gin.H{"request_id": reqID, "cancelled_by": "timeout"}
		Hub.SendToUser(dID, "ride_cancelled", payload)
		Hub.SendToUser(pID, "ride_cancelled", payload)
	}

	reapPassedPickups()
}

// reapPassedPickups proactively expires un-finalized requests (offered or
// pending) whose driver has provably driven past the pickup (fresh location,
// pickup now behind the driver's along-route position). This implements the
// "don't let a passed-by match linger for the full timeout" intent — the slot
// frees immediately. Only acts on drivers with a fresh fix; stale-location
// drivers are left alone.
func reapPassedPickups() {
	rows, err := db.DB.Query(context.Background(), `
		UPDATE ride_requests rr
		SET status = 'expired_passed'
		FROM active_drivers ad
		CROSS JOIN LATERAL (
		    SELECT ST_LineLocatePoint(ad.route::geometry, ad.current_location::geometry) AS df
		) d
		WHERE rr.status IN ('offered','pending')
		  AND ad.driver_id = rr.driver_id
		  AND ad.route IS NOT NULL
		  AND ad.current_location IS NOT NULL
		  AND ad.current_location_at > NOW() - ($1 || ' seconds')::interval
		  AND (
		    SELECT SIGN(pf - d.df) * ST_Length(ST_LineSubstring(
		               ad.route::geometry, LEAST(d.df, pf), GREATEST(d.df, pf)
		           )::geography)
		    FROM ST_LineLocatePoint(ad.route::geometry, rr.pickup_location::geometry) AS pf
		  ) < $2
		RETURNING rr.id, rr.driver_id, rr.passenger_id
	`, strconv.FormatFloat(config.C.MatchLocFreshS, 'f', -1, 64), config.C.MatchMinAheadM)
	if err != nil {
		log.Printf("reaper passed-pickup query: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var reqID, dID, pID string
		if rows.Scan(&reqID, &dID, &pID) != nil {
			continue
		}
		payload := gin.H{"request_id": reqID, "cancelled_by": "driver_passed"}
		Hub.SendToUser(dID, "ride_cancelled", payload)
		Hub.SendToUser(pID, "ride_cancelled", payload)
	}
}
