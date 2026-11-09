package handlers

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
)

// Reverse trigger (Round 2 §5): corridor matching used to run only on driver
// route-set and driver movement. A passenger appearing while every nearby
// driver is parked was invisible until the driver's next location update —
// which may never come. On passenger create/update we now run a targeted
// check the other way: passenger → plausible looking drivers.
//
// Per-passenger throttle mirrors the driver-side shouldRerunCorridor:
// in-process map (single backend instance), small time floor against GPS
// jitter, distance floor so a stationary passenger doesn't re-scan on every
// 15 s location ping. Swept by the reaper alongside the driver map.
var (
	paxRerunMu   sync.Mutex
	paxRerunLast = map[string]rerunMark{}
)

func shouldRerunPassengerMatch(passengerID string, lat, lng float64) bool {
	paxRerunMu.Lock()
	defer paxRerunMu.Unlock()
	now := time.Now()
	if last, ok := paxRerunLast[passengerID]; ok {
		if now.Sub(last.at) < corridorRerunMinInterval {
			return false
		}
		if services.HaversineM(last.lat, last.lng, lat, lng) < config.C.MatchRerunMovedM {
			return false
		}
	}
	paxRerunLast[passengerID] = rerunMark{at: now, lat: lat, lng: lng}
	return true
}

// shouldRerunPassengerMatchTimeOnly gates on the time floor but not on
// movement — used when the passenger's matching inputs changed without their
// position doing so (destination set via /passenger/search, relay leg-2
// kickoff). Still records the run so the next location ping doesn't
// immediately re-scan.
func shouldRerunPassengerMatchTimeOnly(passengerID string, lat, lng float64) bool {
	paxRerunMu.Lock()
	defer paxRerunMu.Unlock()
	now := time.Now()
	if last, ok := paxRerunLast[passengerID]; ok && now.Sub(last.at) < corridorRerunMinInterval {
		return false
	}
	paxRerunLast[passengerID] = rerunMark{at: now, lat: lat, lng: lng}
	return true
}

func sweepPassengerRerunThrottle() {
	cutoff := time.Now().Add(-time.Hour)
	paxRerunMu.Lock()
	for id, mark := range paxRerunLast {
		if mark.at.Before(cutoff) {
			delete(paxRerunLast, id)
		}
	}
	paxRerunMu.Unlock()
}

// reverseMatchForPassenger finds looking drivers whose corridor could
// plausibly include this passenger and runs the standard corridor check
// (same ETA-window / walk / carry gates) restricted to them. Matches flow
// through the exact notify path SetRoute uses, so offers stay idempotent and
// the driver's live counters update. Runs in a detached goroutine.
func reverseMatchForPassenger(passengerID string) {
	ctx := context.Background()

	// Plausibility pre-filter: fresh looking drivers whose stored route
	// passes near the passenger. GREATEST(corridor, long-walk cap) matches
	// the bounding radius the forward query uses for its candidate net.
	rows, err := db.DB.Query(ctx, `
		SELECT ad.driver_id,
		       COALESCE(ST_AsText(ad.route), ''),
		       ST_Y(ad.destination::geometry), ST_X(ad.destination::geometry),
		       ad.corridor_km, ad.seats
		FROM active_drivers ad
		JOIN active_passengers ap ON ap.passenger_id = $1
		WHERE ad.status = 'looking'
		  AND ad.route IS NOT NULL
		  AND ad.current_location IS NOT NULL
		  AND ad.current_location_at > NOW() - make_interval(secs => $2::float8)
		  AND ad.driver_id <> $1
		  AND NOT EXISTS (
		      SELECT 1 FROM user_blocks b
		      WHERE (b.blocker_id = $1 AND b.blocked_id = ad.driver_id)
		         OR (b.blocker_id = ad.driver_id AND b.blocked_id = $1)
		  )
		  -- Constant radius: the corridor query only matches a pickup within
		  -- the walk cap (≤ $3), and a constant lets the GIST index on route
		  -- prefilter by bounding box (the same index condition ST_DWithin
		  -- on geography generates). The old GREATEST(corridor_km*1000, …)
		  -- radius forced a seq scan.
		  AND ad.route && _ST_Expand(ap.location, $3::float8)
		  -- Exact check and ordering on the stored Web Mercator route:
		  -- planar distance divided by the local scale factor 1/cos(lat) is
		  -- metres to well under 1 % over 2 km, and is far cheaper than
		  -- spheroid distance on long routes. Audit bench at 5k: 332 ms
		  -- (spheroid filter + order) → 30 ms, same drivers, same order.
		  AND ST_DWithin(ad.route_3857, ST_Transform(ap.location::geometry, 3857),
		                 $3::float8 / cos(radians(ST_Y(ap.location::geometry))))
		-- Nearest first, so the cap keeps the most relevant drivers instead
		-- of an arbitrary subset.
		ORDER BY ST_Distance(ad.route_3857, ST_Transform(ap.location::geometry, 3857))
		LIMIT $4
	`, passengerID, config.C.MatchLocFreshS, config.C.MatchWalkLongM,
		config.C.MatchPaxTriggerMaxDrivers)
	if err != nil {
		log.Printf("reverseMatchForPassenger %s: %v", passengerID, err)
		return
	}
	type drv struct {
		id, routeWKT     string
		destLat, destLng float64
		corridorKm       float64
		seats            int
	}
	var drivers []drv
	for rows.Next() {
		var d drv
		if rows.Scan(&d.id, &d.routeWKT, &d.destLat, &d.destLng, &d.corridorKm, &d.seats) == nil && d.routeWKT != "" {
			drivers = append(drivers, d)
		}
	}
	rows.Close()

	// Two passes, deliberately.
	//
	// The detour fallback used to run INSIDE this loop, once per unmatched
	// driver. Each pass costs one baseline OSRM route plus up to
	// MatchDetourMaxCandidates via-routes, all serial — so a single passenger
	// location ping could fan out to ~40 sequential OSRM calls (10 drivers × 4)
	// and, at the 8s per-call timeout, occupy a goroutine for minutes. Against
	// the public demo server that is a self-inflicted rate-limit.
	//
	// Corridor matching first (cheap, PostGIS-only), then at most ONE detour
	// pass for the best remaining candidate.
	var unmatched []string
	for _, d := range drivers {
		matches, err := services.FindCorridorMatchForPassenger(
			d.id, d.routeWKT, d.corridorKm, d.destLat, d.destLng, passengerID)
		if err != nil {
			log.Printf("reverseMatchForPassenger %s driver %s: %v", passengerID, d.id, err)
			continue
		}
		notified := 0
		if len(matches) > 0 {
			notified = notifyCorridorMatches(d.id, matches, d.destLat, d.destLng, d.corridorKm, d.seats, false)
		}
		// Only ping the driver's counters when something actually changed —
		// a stats event per parked driver per passenger ping would be noise.
		if notified > 0 {
			emitCorridorStats(d.id, len(matches), notified)
			// A real offer went out; the passenger is engaged, so there is
			// nothing left for the detour fallback to improve on.
			return
		}
		unmatched = append(unmatched, d.id)
	}

	// Walk-only near misses (§2 fallback) for the closest driver that had no
	// corridor match. `drivers` is ordered by the query, so the first unmatched
	// entry is the most plausible one.
	if len(unmatched) > 0 {
		runMicroDetourPass(unmatched[0])
	}
}
