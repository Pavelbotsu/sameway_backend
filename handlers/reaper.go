package handlers

import (
	"context"
	"log"
	"strconv"
	"time"

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
}
