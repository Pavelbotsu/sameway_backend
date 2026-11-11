package handlers

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
)

// Nearby-driver hints. Passengers used to poll /passenger/nearby-drivers
// every 10 s — the heaviest query the server runs (audit P1-1). Now the
// server tells passengers near a driver when that driver's picture changes
// (route set or ended, or moved noticeably) with a tiny `nearby_changed`
// WebSocket event, and the app refreshes on it; its timer is only a slow
// fallback.
const (
	nearbyHintRadiusM      = 10_000 // the app's nearby search radius
	nearbyHintMovedM       = 500
	nearbyHintMinGap       = 30 * time.Second
	nearbyHintMaxReceivers = 500
)

type nearbyHintMark struct {
	at       time.Time
	lat, lng float64
}

var (
	nearbyHintMu   sync.Mutex
	nearbyHintLast = map[string]nearbyHintMark{}
)

// hintNearbyOnMove is called on every driver location update: it hints when
// the driver moved at least nearbyHintMovedM since the last hint, at most
// once per nearbyHintMinGap.
func hintNearbyOnMove(driverID string, lat, lng float64) {
	nearbyHintMu.Lock()
	last, ok := nearbyHintLast[driverID]
	now := time.Now()
	if ok && (now.Sub(last.at) < nearbyHintMinGap ||
		services.HaversineM(last.lat, last.lng, lat, lng) < nearbyHintMovedM) {
		nearbyHintMu.Unlock()
		return
	}
	nearbyHintLast[driverID] = nearbyHintMark{at: now, lat: lat, lng: lng}
	nearbyHintMu.Unlock()
	goMatch("nearby-hint", func() { sendNearbyHints(lat, lng) })
}

// hintNearbyNow hints regardless of movement: a route was published at
// (lat, lng).
func hintNearbyNow(driverID string, lat, lng float64) {
	nearbyHintMu.Lock()
	nearbyHintLast[driverID] = nearbyHintMark{at: time.Now(), lat: lat, lng: lng}
	nearbyHintMu.Unlock()
	goMatch("nearby-hint", func() { sendNearbyHints(lat, lng) })
}

// hintNearbyRouteEnded hints around the driver's last known position when
// they end their route, so passengers drop them from the map.
func hintNearbyRouteEnded(driverID string) {
	nearbyHintMu.Lock()
	last, ok := nearbyHintLast[driverID]
	delete(nearbyHintLast, driverID)
	nearbyHintMu.Unlock()
	if ok {
		goMatch("nearby-hint", func() { sendNearbyHints(last.lat, last.lng) })
	}
}

func sendNearbyHints(lat, lng float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := db.DB.Query(ctx, `
		SELECT passenger_id::text FROM active_passengers
		WHERE ST_DWithin(location, ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography, $3)
		  AND updated_at > NOW() - INTERVAL '10 minutes'
		LIMIT $4`, lng, lat, nearbyHintRadiusM, nearbyHintMaxReceivers)
	if err != nil {
		log.Printf("nearby hints: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			Hub.SendToUser(id, "nearby_changed", gin.H{})
		}
	}
}

// sweepNearbyHintThrottle drops marks idle for over an hour (reaper tick).
func sweepNearbyHintThrottle() {
	cutoff := time.Now().Add(-time.Hour)
	nearbyHintMu.Lock()
	for id, m := range nearbyHintLast {
		if m.at.Before(cutoff) {
			delete(nearbyHintLast, id)
		}
	}
	nearbyHintMu.Unlock()
}
