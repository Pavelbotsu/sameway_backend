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
		ReapStalePendingOnce(timeoutMinutes)
	}
}

// ReapStalePendingOnce performs a single sweep. Exported so tests can invoke
// it synchronously without depending on the ticker.
func ReapStalePendingOnce(timeoutMinutes int) {
	rows, err := db.DB.Query(context.Background(), `
		UPDATE ride_requests
		SET status = 'cancelled'
		WHERE status = 'pending'
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

// reapPassedPickups proactively expires pending requests whose driver has
// provably driven past the pickup (fresh location, pickup now behind the
// driver's along-route position). This implements the "don't let a passed-by
// match linger for the full timeout" intent — the slot frees immediately.
// Only acts on drivers with a fresh fix; stale-location drivers are left alone.
func reapPassedPickups() {
	rows, err := db.DB.Query(context.Background(), `
		UPDATE ride_requests rr
		SET status = 'expired_passed'
		FROM active_drivers ad
		WHERE rr.status = 'pending'
		  AND ad.driver_id = rr.driver_id
		  AND ad.route IS NOT NULL
		  AND ad.current_location IS NOT NULL
		  AND ad.current_location_at > NOW() - ($1 || ' seconds')::interval
		  AND (
		        ST_LineLocatePoint(ad.route::geometry, rr.pickup_location::geometry)
		      - ST_LineLocatePoint(ad.route::geometry, ad.current_location::geometry)
		  ) * ST_Length(ad.route) < $2
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
