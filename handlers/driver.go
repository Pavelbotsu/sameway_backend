package handlers

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/services/ranker"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Per-driver throttle for the on-location-update corridor re-run. In-process
// only — single backend instance assumed. Move to Redis if we ever scale out.
//
// Throttle is by DISTANCE TRAVELLED, not wall-clock: a 30 s timer is ~830 m of
// blind spot at 100 km/h. We re-run once the driver has moved ≥ MatchRerunMovedM
// since the last run (with a small time floor so GPS jitter while parked doesn't
// spam the matcher).
var (
	corridorRerunMu   sync.Mutex
	corridorRerunLast = map[string]rerunMark{}
)

type rerunMark struct {
	at       time.Time
	lat, lng float64
}

const corridorRerunMinInterval = 5 * time.Second

func shouldRerunCorridor(driverID string, lat, lng float64) bool {
	corridorRerunMu.Lock()
	defer corridorRerunMu.Unlock()
	now := time.Now()
	if last, ok := corridorRerunLast[driverID]; ok {
		if now.Sub(last.at) < corridorRerunMinInterval {
			return false
		}
		if services.HaversineM(last.lat, last.lng, lat, lng) < config.C.MatchRerunMovedM {
			return false
		}
	}
	corridorRerunLast[driverID] = rerunMark{at: now, lat: lat, lng: lng}
	return true
}

// sweepCorridorRerunThrottle drops marks idle for over an hour so the map
// doesn't accumulate one entry per driver ever seen. Called from the reaper
// tick.
func sweepCorridorRerunThrottle() {
	cutoff := time.Now().Add(-time.Hour)
	corridorRerunMu.Lock()
	for id, mark := range corridorRerunLast {
		if mark.at.Before(cutoff) {
			delete(corridorRerunLast, id)
		}
	}
	corridorRerunMu.Unlock()
}

// emitCorridorStats pushes the driver's live matching counters over WS after
// every corridor run (route set, movement re-run, passenger-appearance reverse
// trigger). The driver UI renders these instead of the frozen SetRoute
// response, fixing the "0 passengers notified never updates" bug. Best-effort:
// a failed count query just skips the event, never the matching itself.
func emitCorridorStats(driverID string, candidatesFound, newlyNotified int) {
	// total_notified is cumulative for the CURRENT route (active_drivers.
	// created_at resets on every route upsert), so the client can render it
	// directly — summing per-event deltas client-side would double-count
	// whenever an event races the SetRoute HTTP response.
	var pending, totalNotified int
	if err := db.DB.QueryRow(context.Background(), `
		SELECT
			COUNT(*) FILTER (WHERE rr.status IN ('offered','pending')),
			COUNT(*) FILTER (WHERE rr.created_at >= ad.created_at)
		FROM ride_requests rr
		JOIN active_drivers ad ON ad.driver_id = rr.driver_id
		WHERE rr.driver_id = $1
	`, driverID).Scan(&pending, &totalNotified); err != nil {
		log.Printf("emitCorridorStats %s: %v", driverID, err)
		return
	}
	Hub.SendToUser(driverID, "corridor_stats", gin.H{
		"candidates_found":       candidatesFound,
		"new_notifications_sent": newlyNotified,
		"active_pending_count":   pending,
		"total_notified":         totalNotified,
	})
}

// rerunCorridorMatch re-runs FindPassengersInCorridor with the driver's stored
// route + destination + corridor and notifies any newly-eligible passengers.
// Called from a goroutine in UpdateLocation; uses background context so a
// dropped HTTP connection doesn't cut the work short. Idempotent.
func rerunCorridorMatch(driverID string) {
	ctx := context.Background()
	var (
		routeWKT         string
		destLat, destLng float64
		corridorKm       float64
		seats            int
	)
	err := db.DB.QueryRow(ctx, `
		SELECT COALESCE(ST_AsText(route), ''),
		       ST_Y(destination::geometry), ST_X(destination::geometry),
		       corridor_km, seats
		FROM active_drivers
		WHERE driver_id = $1 AND status = 'looking'
	`, driverID).Scan(&routeWKT, &destLat, &destLng, &corridorKm, &seats)
	if err != nil || routeWKT == "" {
		return
	}
	matches, err := services.FindPassengersInCorridor(
		driverID, routeWKT, corridorKm,
		destLat, destLng,
		config.C.MaxCorridorNotifications,
	)
	if err != nil {
		log.Printf("rerunCorridorMatch %s: %v", driverID, err)
		return
	}
	notified := 0
	if len(matches) > 0 {
		notified = notifyCorridorMatches(driverID, matches, destLat, destLng, corridorKm, seats, false)
	}
	// Zero-match runs emit too — the counter must stay honest while parked.
	emitCorridorStats(driverID, len(matches), notified)
	// Already on a background goroutine — run the detour fallback inline.
	runMicroDetourPass(driverID)
}

func SetRoute(c *gin.Context) {
	// Pointer coords: `required` on plain floats rejects legal zero values
	// (equator / Greenwich meridian).
	var req struct {
		OriginLat      *float64 `json:"origin_lat" binding:"required"`
		OriginLng      *float64 `json:"origin_lng" binding:"required"`
		DestinationLat *float64 `json:"destination_lat" binding:"required"`
		DestinationLng *float64 `json:"destination_lng" binding:"required"`
		CorridorKm     float64  `json:"corridor_km" binding:"required,gt=0,max=50"`
		Seats          int      `json:"seats" binding:"required,min=1,max=8"`
		PreferAI       bool     `json:"prefer_ai"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validLatLng(*req.OriginLat, *req.OriginLng) || !validLatLng(*req.DestinationLat, *req.DestinationLng) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coordinates out of range"})
		return
	}
	oLat, oLng := *req.OriginLat, *req.OriginLng
	dLat, dLng := *req.DestinationLat, *req.DestinationLng

	driverID := c.GetString("user_id")
	if !driverReady(c.Request.Context(), c, driverID) {
		return
	}

	route, err := services.GetRoadRoute(oLat, oLng, dLat, dLng)
	if err != nil {
		log.Printf("SetRoute: routing for %s: %v", driverID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "routing failed"})
		return
	}

	routeMEWKT := services.BuildMeasuredLineEWKT(route.Coords, route.Durations)
	if err := services.UpsertActiveDriver(driverID, route.WKT, routeMEWKT,
		oLat, oLng, dLat, dLng,
		req.CorridorKm, req.Seats); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	matches, err := services.FindPassengersInCorridor(
		driverID, route.WKT, req.CorridorKm,
		dLat, dLng,
		config.C.MaxCorridorNotifications,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "geo query failed"})
		return
	}
	log.Printf("SetRoute driver=%s matched %d passengers in corridor (corridor_km=%.2f limit=%d)",
		driverID, len(matches), req.CorridorKm, config.C.MaxCorridorNotifications)

	notified := notifyCorridorMatches(
		driverID, matches,
		dLat, dLng,
		req.CorridorKm, req.Seats, req.PreferAI,
	)
	emitCorridorStats(driverID, len(matches), notified)
	// Micro-detour fallback (§2): opt-in, near-miss-only, async — an OSRM
	// round-trip must not sit on the route-set response.
	goMatch("micro-detour-pass", func() { runMicroDetourPass(driverID) })

	c.JSON(http.StatusOK, gin.H{
		"route_wkt":      route.WKT,
		"distance_km":    route.DistanceKm,
		"notified_count": notified,
	})
}

// notifyCorridorMatches reserves a ride_request row for each match (idempotent
// via CreateRideRequest) and emits the `ride_request` WS event + FCM push to
// passengers we hadn't already notified. Returns the count of fresh
// notifications. Shared between SetRoute and the throttled re-run in
// UpdateLocation.
//
// preferAI is a per-call opt-in. When true and ranker.Active is an AI ranker,
// matches are reordered by AI score and tagged with `match_method` on the WS
// payload. On any AI failure the order falls back to the SQL-original
// (geometric) and the tag becomes "geometric_fallback".
func notifyCorridorMatches(
	driverID string,
	matches []services.CorridorMatch,
	destLat, destLng, corridorKm float64, seats int,
	preferAI bool,
) int {
	var (
		driverName, carMake, carModel, carColor, photoURL string
		driverAvgRating                                   float64
		driverRatingCount                                 int
	)
	// Offers go to up to MaxCorridorNotifications passengers before anyone has
	// accepted, so they carry the same reduced identity as nearby-drivers:
	// first name, no plate (driverRespondAccept sends the full details to the
	// one passenger who gets the ride).
	if err := db.DB.QueryRow(context.Background(), `
		SELECT split_part(u.name, ' ', 1),
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''),
		       COALESCE(u.photo_url,''),
		       COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name, u.car_make, u.car_model, u.car_color, u.photo_url
	`, driverID).Scan(&driverName, &carMake, &carModel, &carColor, &photoURL, &driverAvgRating, &driverRatingCount); err != nil {
		log.Printf("notifyCorridorMatches: driver profile %s: %v", driverID, err)
	}

	ordered, methodTag := rankAndReorder(matches, driverID, corridorKm, seats, preferAI)

	notified := 0
	for _, m := range ordered {
		// Corridor offers are created as 'offered' (awaiting the passenger's
		// accept). The passenger's accept promotes to 'pending' and notifies
		// the driver, who confirms — so the driver always decides.
		details := services.RideRequestDetails{MatchKind: m.MatchKind}
		if m.MatchKind == "partial_relay" {
			dLat, dLng := m.DropoffLat, m.DropoffLng
			details.DropoffLat, details.DropoffLng = &dLat, &dLng
		}
		if m.MatchKind == "micro_detour" {
			addM, addS := m.DetourAddedM, m.DetourAddedS
			details.DetourAddedM, details.DetourAddedS = &addM, &addS
		}
		requestID, isNew, err := services.CreateRideRequestFull(
			driverID, m.User.ID, m.PickupLat, m.PickupLng, models.RequestOffered, details)
		if err != nil || !isNew {
			continue
		}
		payload := gin.H{
			"request_id":          requestID,
			"driver_id":           driverID,
			"driver_name":         driverName,
			"driver_photo_url":    photoURL,
			"driver_avg_rating":   driverAvgRating,
			"driver_rating_count": driverRatingCount,
			"car_make":            carMake,
			"car_model":           carModel,
			"car_color":           carColor,
			// Coarse: the passenger only needs "is the driver heading my
			// way", not the address the driver is going to.
			"destination_lat":             services.SnapToPrivacyGrid(destLat),
			"destination_lng":             services.SnapToPrivacyGrid(destLng),
			"corridor_km":                 corridorKm,
			"seats":                       seats,
			"pickup_lat":                  m.PickupLat,
			"pickup_lng":                  m.PickupLng,
			"walk_distance_m":             m.WalkM,
			"driver_distance_to_pickup_m": m.DriverToPickupM,
			"match_method":                methodTag,
			"match_kind":                  m.MatchKind,
		}
		if m.MatchKind == "partial_relay" {
			// The passenger opts into "ride to X, then continue" BEFORE
			// accepting — the offer card needs the dropoff and what the
			// continuation looks like (stop nearby, or a fresh search).
			payload["dropoff_lat"] = m.DropoffLat
			payload["dropoff_lng"] = m.DropoffLng
			payload["transit_stop_name"] = m.TransitStopName
			payload["transit_stop_dist_m"] = m.TransitStopDistM
			payload["relay_fallback"] = m.TransitStopName == ""
		}
		if m.MatchKind == "micro_detour" {
			// Never silent: both sides see the delta the driver accepts.
			payload["detour_added_m"] = m.DetourAddedM
			payload["detour_added_s"] = m.DetourAddedS
		}
		// FCM is a synchronous HTTPS round-trip; up to 20 of them serially
		// would add seconds to SetRoute. Fire-and-forget with recovery.
		fcmToken := m.User.FCMToken
		go safeGo("fcm-corridor-notify", func() {
			services.NotifyPassengerRideAvailable(fcmToken, driverID, destLat, destLng)
		})()
		Hub.SendToUser(m.User.ID, "ride_request", payload)
		notified++

		kind, paxID, reqID := m.MatchKind, m.User.ID, requestID
		go safeGo("match-event-offered", func() {
			logMatchEventKnownKind(reqID, kind, "offered", map[string]any{
				"walk_m": m.WalkM, "carry_m": m.CarryM, "eta_s": m.EtaPickupS,
			})
			// Relay leg-2 labeling: if this passenger finished a partial_relay
			// ride in the last 30 minutes, this offer IS the second leg — log
			// it on the ORIGINAL relay request so the funnel joins up.
			var priorRelayID string
			if err := db.DB.QueryRow(context.Background(), `
				SELECT id FROM ride_requests
				WHERE passenger_id = $1 AND match_kind = 'partial_relay'
				  AND status = 'done'
				  AND created_at > NOW() - INTERVAL '30 minutes'
				ORDER BY created_at DESC LIMIT 1
			`, paxID).Scan(&priorRelayID); err == nil {
				logMatchEventKnownKind(priorRelayID, "partial_relay", "leg2_offer_created",
					map[string]any{"leg2_request_id": reqID})
			}
		})()
	}
	return notified
}

// rankAndReorder runs the configured ranker over the matches and returns them
// reordered by descending score, plus the method tag for the WS payload. If
// preferAI is false or ranker.Active is the geometric passthrough, the input
// order is preserved. Errors are swallowed (fall back to the SQL ordering and
// tag the result as "geometric_fallback").
func rankAndReorder(
	matches []services.CorridorMatch,
	driverID string, corridorKm float64, seats int,
	preferAI bool,
) ([]services.CorridorMatch, string) {
	if len(matches) <= 1 {
		return matches, ranker.GeometricRanker{}.Name()
	}
	chosen := ranker.MatchRanker(ranker.GeometricRanker{})
	if preferAI {
		chosen = ranker.Active
	}
	cands := make([]ranker.Candidate, len(matches))
	passengerIDs := make([]string, len(matches))
	for i, m := range matches {
		cands[i] = ranker.Candidate{
			UserID:          m.User.ID,
			Name:            m.User.Name,
			WalkM:           m.WalkM,
			DriverToPickupM: m.DriverToPickupM,
		}
		passengerIDs[i] = m.User.ID
	}
	// Best-effort batch rating lookup so the LLM has more to weigh than
	// raw distance. Falls back to 0/0 on error — the ranker handles that.
	if ratings, err := fetchPassengerRatings(passengerIDs); err == nil {
		for i := range cands {
			if r, ok := ratings[cands[i].UserID]; ok {
				cands[i].AvgRating = r.Avg
				cands[i].RatingCount = r.Count
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results, err := chosen.Rank(ctx, cands, ranker.RankContext{
		DriverID: driverID, CorridorKm: corridorKm, Seats: seats,
	})
	if err != nil {
		log.Printf("ranker %s failed: %v — falling back to geometric", chosen.Name(), err)
		return matches, "geometric_fallback"
	}
	// Reorder by results, preserving original entries that the ranker omitted.
	posByID := make(map[string]int, len(results))
	for i, r := range results {
		posByID[r.UserID] = i
	}
	ordered := make([]services.CorridorMatch, len(matches))
	copy(ordered, matches)
	slices.SortStableFunc(ordered, func(a, b services.CorridorMatch) int {
		ai, aok := posByID[a.User.ID]
		bi, bok := posByID[b.User.ID]
		switch {
		case aok && bok:
			return cmp.Compare(ai, bi)
		case aok:
			return -1
		case bok:
			return 1
		}
		return 0
	})
	return ordered, chosen.Name()
}

type ratingAgg struct {
	Avg   float64
	Count int
}

func fetchPassengerRatings(ids []string) (map[string]ratingAgg, error) {
	out := make(map[string]ratingAgg, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.DB.Query(context.Background(), `
		SELECT rated_id, COALESCE(AVG(stars), 0), COUNT(id)
		FROM ratings
		WHERE rated_id = ANY($1)
		GROUP BY rated_id
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var agg ratingAgg
		if err := rows.Scan(&id, &agg.Avg, &agg.Count); err != nil {
			return nil, err
		}
		out[id] = agg
	}
	return out, nil
}

func UpdateLocation(c *gin.Context) {
	// Pointers so latitude/longitude 0 (equator / Greenwich meridian) bind
	// correctly — `binding:"required"` on a plain float rejects zero values.
	var req struct {
		Lat *float64 `json:"lat" binding:"required"`
		Lng *float64 `json:"lng" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validLatLng(*req.Lat, *req.Lng) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coordinates out of range"})
		return
	}
	lat, lng := *req.Lat, *req.Lng
	driverID := c.GetString("user_id")

	// current_location is what the ENTIRE no-passed-by system keys off
	// (corridor match freshness gate, accept re-validation, reaper). A silent
	// failure here quietly degrades matching, so it is written first and
	// logged on error.
	if _, err := db.DB.Exec(c.Request.Context(), `
		UPDATE active_drivers
		SET current_location = ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography,
		    current_location_at = NOW()
		WHERE driver_id = $3
	`, lng, lat, driverID); err != nil {
		log.Printf("UpdateLocation: mirror for %s: %v", driverID, err)
	}

	// As the driver moves, passengers who were previously "behind" or "out of
	// early-notify range" may become eligible. Re-run the corridor match —
	// idempotent CreateRideRequest means already-notified passengers are
	// skipped automatically. Throttled by distance moved (MatchRerunMovedM)
	// with a small time floor — see shouldRerunCorridor.
	if shouldRerunCorridor(driverID, lat, lng) {
		goMatch("rerunCorridorMatch", func() { rerunCorridorMatch(driverID) })
	}

	// Live location: relay to the counterparty passenger(s) of any active ride
	// (accepted = approaching pickup, in_progress = in the car) so their map
	// tracks the car. Throttled per ride; only the counterparty receives it;
	// terminal statuses fall out of this query, so the stream tears down on its
	// own.
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT id, passenger_id FROM ride_requests
		WHERE driver_id = $1 AND status IN ('accepted','in_progress')
	`, driverID)
	if err == nil {
		for rows.Next() {
			var reqID, passengerID string
			if rows.Scan(&reqID, &passengerID) == nil && shouldRelayLiveLoc(reqID+":d") {
				Hub.SendToUser(passengerID, "driver_location", gin.H{
					"request_id": reqID,
					"lat":        lat,
					"lng":        lng,
				})
			}
		}
		rows.Close()
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func GetRequests(c *gin.Context) {
	driverID := c.GetString("user_id")
	requests, err := services.GetDriverRequests(driverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if requests == nil {
		requests = []models.RideRequest{}
	}
	c.JSON(http.StatusOK, requests)
}

func UpdateStatus(c *gin.Context) {
	var req struct {
		Status     string  `json:"status" binding:"required,oneof=looking full in_progress done"`
		DistanceKm float64 `json:"distance_km"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.DistanceKm < 0 || req.DistanceKm > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "distance_km out of range"})
		return
	}
	driverID := c.GetString("user_id")
	if err := services.UpdateDriverStatus(driverID, req.Status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if req.Status == "done" {
		// "End driving day" — settle every remaining active ride in ONE
		// statement so rides accepted concurrently can't slip past the
		// notification loop. Picked-up rides (in_progress) complete; rides
		// still waiting for pickup (accepted) are cancelled, since nothing
		// shows the two ever met. Per-passenger drop-off uses
		// DropoffPassenger instead.
		rows, err := db.DB.Query(context.Background(), `
			WITH done AS (
				UPDATE ride_requests SET status = 'done'
				WHERE driver_id = $1 AND status = 'in_progress'
				RETURNING id, passenger_id, match_kind, TRUE AS completed
			), cancelled AS (
				UPDATE ride_requests SET status = 'cancelled', cancel_reason = 'driver_ended'
				WHERE driver_id = $1 AND status = 'accepted'
				RETURNING id, passenger_id, match_kind, FALSE AS completed
			)
			SELECT * FROM done UNION ALL SELECT * FROM cancelled
		`, driverID)
		if err == nil {
			// Drain the cursor FIRST, then act. The UPDATE has already
			// committed by this point, so anything done while the rows are
			// still streaming (a per-row INSERT, a WS push) holds a pool
			// connection open and — worse — an error mid-stream used to exit
			// the loop silently, leaving rides marked done in the database with
			// no notification, no trip record, and a 200 on the response.
			type doneRide struct {
				ReqID, PassengerID, MatchKind string
				Completed                     bool
			}
			var done []doneRide
			for rows.Next() {
				var d doneRide
				if err := rows.Scan(&d.ReqID, &d.PassengerID, &d.MatchKind, &d.Completed); err != nil {
					log.Printf("UpdateStatus: scan done row for %s: %v", driverID, err)
					continue
				}
				done = append(done, d)
			}
			rowsErr := rows.Err()
			rows.Close()
			if rowsErr != nil {
				// The rides ARE done in the database; we just couldn't read all
				// of them back. Log loudly — the affected passengers will not
				// have received ride_done and will resync via /my-requests.
				log.Printf("UpdateStatus: bulk-done iteration for %s (rides committed, notifications may be incomplete): %v",
					driverID, rowsErr)
			}
			for _, d := range done {
				if !d.Completed {
					Hub.SendToUser(d.PassengerID, "ride_cancelled", gin.H{
						"request_id": d.ReqID, "cancelled_by": "driver_ended",
					})
					logMatchEvent(d.ReqID, "cancelled", map[string]any{"by": "driver", "reason": "driver_ended"})
					continue
				}
				if req.DistanceKm > 0 {
					if err := services.RecordCompletedTrip(d.ReqID, driverID, d.PassengerID, req.DistanceKm); err != nil {
						log.Printf("UpdateStatus: record trip %s: %v", d.ReqID, err)
					}
				}
				payload := gin.H{
					"request_id":   d.ReqID,
					"driver_id":    driverID,
					"passenger_id": d.PassengerID,
				}
				Hub.SendToUser(d.PassengerID, "ride_done", payload)
				Hub.SendToUser(driverID, "ride_done", payload)
				startRelayLeg2IfNeeded(d.ReqID, d.PassengerID, d.MatchKind)
			}
			if _, err := db.DB.Exec(context.Background(),
				`UPDATE active_drivers SET seats_taken = 0 WHERE driver_id = $1`,
				driverID,
			); err != nil {
				log.Printf("UpdateStatus: reset seats for %s: %v", driverID, err)
			}
		} else {
			log.Printf("UpdateStatus: bulk-done for %s: %v", driverID, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// startRelayLeg2IfNeeded kicks off the second leg of a partial-relay ride at
// dropoff (Round 2 §1): the passenger's active_passengers row (destination
// intact) stays in the pool, they get an explicit relay_leg2_search event so
// the UI flips back to "searching for your next leg", and a reverse corridor
// check runs immediately from their current position — no throttle, this is
// the moment they're standing at the dropoff. Also logs the outcome trail.
func startRelayLeg2IfNeeded(requestID, passengerID, matchKind string) {
	logMatchEventKnownKind(requestID, matchKind, "completed", nil)
	if matchKind != "partial_relay" {
		return
	}
	Hub.SendToUser(passengerID, "relay_leg2_search", gin.H{
		"request_id": requestID,
	})
	logMatchEventKnownKind(requestID, matchKind, "leg2_search_started", nil)
	goMatch("relay-leg2-match", func() { reverseMatchForPassenger(passengerID) })
}

// DropoffPassenger ends a single passenger's ride without ending the driver's
// route. Records the completed trip, decrements seats_taken (and flips status
// from 'full' back to 'looking' if applicable), emits ride_done to both
// parties. Other passengers on the same vehicle are unaffected.
func DropoffPassenger(c *gin.Context) {
	var req struct {
		DistanceKm float64 `json:"distance_km"`
	}
	_ = c.ShouldBindJSON(&req) // distance_km is optional; default 0 → no trip record
	if req.DistanceKm < 0 || req.DistanceKm > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "distance_km out of range"})
		return
	}
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	// Lock the ride_request and the active_drivers row together so we never
	// race with /pickup/confirm or /driver/respond.
	var passengerID, status, matchKind string
	err = tx.QueryRow(ctx, `
		SELECT passenger_id, status, match_kind FROM ride_requests
		WHERE id = $1 AND driver_id = $2
		FOR UPDATE
	`, requestID, driverID).Scan(&passengerID, &status, &matchKind)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride not found or not yours"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if status == "accepted" {
		// No pickup code was confirmed, so there is no proof the two met.
		// Completing it would unlock ratings and post-ride chat for a ride
		// that never happened; cancel or report a no-show instead.
		c.JSON(http.StatusConflict, gin.H{
			"error": "passenger not picked up yet: confirm the pickup code, or cancel / report a no-show",
			"code":  "not_picked_up",
		})
		return
	}
	if status != "in_progress" {
		c.JSON(http.StatusConflict, gin.H{"error": "ride is not active"})
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE ride_requests SET status = 'done' WHERE id = $1
	`, requestID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = GREATEST(seats_taken - 1, 0),
		    status      = CASE WHEN status = 'full' THEN 'looking' ELSE status END
		WHERE driver_id = $1
	`, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if req.DistanceKm > 0 {
		services.RecordCompletedTrip(requestID, driverID, passengerID, req.DistanceKm)
	}

	payload := gin.H{
		"request_id":   requestID,
		"driver_id":    driverID,
		"passenger_id": passengerID,
	}
	Hub.SendToUser(passengerID, "ride_done", payload)
	Hub.SendToUser(driverID, "ride_done", payload)
	startRelayLeg2IfNeeded(requestID, passengerID, matchKind)

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// GetPickupRoute returns three OSRM road routes for an accepted ride:
//
//   - pickup_wkt: driver → pickup point on the road (driving)
//   - continuation_wkt: pickup point → driver's destination (driving)
//   - walk_wkt: passenger's current location → pickup point (walking)
//
// The pickup point is taken from `ride_requests.pickup_location` (set at
// match time as the projection of the passenger onto the driver's route), so
// even destination-anchored matches where the passenger is several km off
// the route still get a sensible road-side pickup.
//
// Either the driver or the passenger of the ride may call this — the
// participant check below replaces the older driver-only gate. On OSRM
// failure the failing leg returns wkt:"" and distance:0 so the UI can still
// render whatever legs succeeded.
func GetPickupRoute(c *gin.Context) {
	requestID := c.Param("request_id")
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	var driverID, passengerID, status string
	var pickupLat, pickupLng float64
	err := db.DB.QueryRow(ctx, `
		SELECT driver_id, passenger_id, status,
		       ST_Y(pickup_location::geometry), ST_X(pickup_location::geometry)
		FROM ride_requests
		WHERE id = $1
	`, requestID).Scan(&driverID, &passengerID, &status, &pickupLat, &pickupLng)
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
	if status != "accepted" && status != "in_progress" {
		c.JSON(http.StatusConflict, gin.H{"error": "ride not in pickup phase"})
		return
	}

	var driverLat, driverLng, destLat, destLng float64
	if err := db.DB.QueryRow(ctx, `
		SELECT ST_Y(COALESCE(current_location, origin)::geometry),
		       ST_X(COALESCE(current_location, origin)::geometry),
		       ST_Y(destination::geometry),
		       ST_X(destination::geometry)
		FROM active_drivers WHERE driver_id = $1
	`, driverID).Scan(&driverLat, &driverLng, &destLat, &destLng); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "driver not active"})
		return
	}

	var passengerLat, passengerLng float64
	if err := db.DB.QueryRow(ctx, `
		SELECT ST_Y(location::geometry), ST_X(location::geometry)
		FROM active_passengers WHERE passenger_id = $1
	`, passengerID).Scan(&passengerLat, &passengerLng); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "passenger location unknown"})
		return
	}

	// The three OSRM legs are independent — fetch them in parallel so the
	// endpoint costs one round-trip, not three sequential ones.
	var (
		pickupWkt, continuationWkt, walkWkt string
		pickupKm, continuationKm, walkKm    float64
		wg                                  sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		if r, err := services.GetRoute("driving", driverLat, driverLng, pickupLat, pickupLng); err == nil {
			pickupWkt, pickupKm = r.WKT, r.DistanceKm
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := services.GetRoute("driving", pickupLat, pickupLng, destLat, destLng); err == nil {
			continuationWkt, continuationKm = r.WKT, r.DistanceKm
		}
	}()
	go func() {
		defer wg.Done()
		if r, err := services.GetRoute("foot", passengerLat, passengerLng, pickupLat, pickupLng); err == nil {
			walkWkt, walkKm = r.WKT, r.DistanceKm
		}
	}()
	wg.Wait()

	c.JSON(http.StatusOK, gin.H{
		"pickup_wkt":               pickupWkt,
		"pickup_distance_km":       pickupKm,
		"continuation_wkt":         continuationWkt,
		"continuation_distance_km": continuationKm,
		"walk_wkt":                 walkWkt,
		"walk_distance_km":         walkKm,
	})
}

func DriverCancelRide(c *gin.Context) {
	requestID := c.Param("request_id")
	driverID := c.GetString("user_id")
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	var passengerID string
	err = tx.QueryRow(ctx,
		`UPDATE ride_requests SET status = 'cancelled'
		 WHERE id = $1 AND driver_id = $2 AND status = 'accepted'
		 RETURNING passenger_id`,
		requestID, driverID,
	).Scan(&passengerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ride not found or already cancelled"})
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = GREATEST(seats_taken - 1, 0),
		    status = CASE WHEN status = 'full' THEN 'looking' ELSE status END
		WHERE driver_id = $1
	`, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
		"request_id":   requestID,
		"cancelled_by": "driver",
	})
	go safeGo("match-event", func() {
		logMatchEvent(requestID, "cancelled", map[string]any{"by": "driver"})
	})()

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func DeleteRoute(c *gin.Context) {
	driverID := c.GetString("user_id")

	// 'offered' included: a dangling offer would otherwise let a passenger
	// accept a ride from a driver who already ended their route.
	rows, err := db.DB.Query(context.Background(),
		`UPDATE ride_requests
		 SET status = 'cancelled'
		 WHERE driver_id = $1 AND status IN ('offered','pending','accepted')
		 RETURNING id, passenger_id`,
		driverID,
	)
	if err == nil {
		var cancelled []string
		type cancelledRide struct{ ReqID, PassengerID string }
		var toNotify []cancelledRide
		for rows.Next() {
			var cr cancelledRide
			if err := rows.Scan(&cr.ReqID, &cr.PassengerID); err != nil {
				log.Printf("DeleteRoute: scan cancelled row for %s: %v", driverID, err)
				continue
			}
			toNotify = append(toNotify, cr)
			cancelled = append(cancelled, cr.ReqID)
		}
		if err := rows.Err(); err != nil {
			log.Printf("DeleteRoute: iteration for %s (rows cancelled, notifications may be incomplete): %v",
				driverID, err)
		}
		rows.Close()
		for _, cr := range toNotify {
			Hub.SendToUser(cr.PassengerID, "ride_cancelled", gin.H{
				"request_id":   cr.ReqID,
				"cancelled_by": "driver",
			})
		}
		go safeGo("match-event", func() {
			for _, id := range cancelled {
				logMatchEvent(id, "cancelled", map[string]any{"by": "driver_route_deleted"})
			}
		})()
	}

	if err := services.DeleteActiveDriver(driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DriverRespondToRide mirrors RespondToRide on the passenger side. When a
// passenger has sent a request via RequestDriver, the driver uses this
// endpoint to accept or decline. Accept path runs inside a pgx transaction
// matching respondAccept (seat reservation + auto-cancel competing rows).
func DriverRespondToRide(c *gin.Context) {
	var req struct {
		RequestID string `json:"request_id" binding:"required"`
		Status    string `json:"status" binding:"required,oneof=accepted declined"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	driverID := c.GetString("user_id")

	if req.Status == "declined" {
		driverRespondDecline(c, driverID, req.RequestID)
		return
	}
	driverRespondAccept(c, driverID, req.RequestID)
}

func driverRespondDecline(c *gin.Context, driverID, requestID string) {
	var passengerID string
	err := db.DB.QueryRow(c.Request.Context(), `
		UPDATE ride_requests SET status = 'declined'
		WHERE id = $1 AND driver_id = $2 AND status = 'pending'
		RETURNING passenger_id
	`, requestID, driverID).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request not pending"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// No plate: it identifies the driver's car and only the passenger of an
	// ACCEPTED ride needs it (driverRespondAccept sends it).
	name, avg, count, carMake, carModel, carColor, _, photoURL := loadDriverProfile(driverID)
	Hub.SendToUser(passengerID, "driver_response", gin.H{
		"request_id":          requestID,
		"driver_id":           driverID,
		"driver_name":         name,
		"driver_photo_url":    photoURL,
		"driver_avg_rating":   avg,
		"driver_rating_count": count,
		"car_make":            carMake,
		"car_model":           carModel,
		"car_color":           carColor,
		"status":              "declined",
	})
	go safeGo("match-event", func() { logMatchEvent(requestID, "driver_declined", nil) })()

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func driverRespondAccept(c *gin.Context, driverID, requestID string) {
	ctx := c.Request.Context()

	tx, err := db.DB.Begin(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback(ctx)

	// 1) Lock the pending request and identify the passenger.
	var passengerID string
	err = tx.QueryRow(ctx, `
		SELECT passenger_id FROM ride_requests
		WHERE id = $1 AND driver_id = $2 AND status = 'pending'
		FOR UPDATE
	`, requestID, driverID).Scan(&passengerID)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusConflict, gin.H{"error": "request not pending"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 1b) Re-validate position before reserving a seat: if the driver has
	// provably passed the pickup since the match, expire the row and free the
	// passenger's slot rather than accepting an impossible ride.
	if aheadM, ok := pickupStillAhead(ctx, tx, requestID); ok && aheadM < config.C.MatchMinAheadM {
		if expirePassedRide(ctx, tx, requestID, passengerID) {
			c.JSON(http.StatusConflict, gin.H{"error": "you have already passed this pickup"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 2) Lock the driver's own capacity row, validate & reserve a seat.
	var seats, taken int
	err = tx.QueryRow(ctx, `
		SELECT seats, seats_taken FROM active_drivers
		WHERE driver_id = $1 AND status IN ('looking','full')
		FOR UPDATE
	`, driverID).Scan(&seats, &taken)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusGone, gin.H{"error": "no active route"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if taken >= seats {
		c.JSON(http.StatusConflict, gin.H{"error": "ride is full"})
		return
	}
	newTaken := taken + 1
	newStatus := "looking"
	if newTaken >= seats {
		newStatus = "full"
	}
	if _, err = tx.Exec(ctx, `
		UPDATE active_drivers
		SET seats_taken = $1, status = $2
		WHERE driver_id = $3
	`, newTaken, newStatus, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 3) Flip THIS request to accepted.
	if _, err = tx.Exec(ctx, `
		UPDATE ride_requests SET status = 'accepted' WHERE id = $1
	`, requestID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 4) Auto-cancel the PASSENGER's other active engagements (dangling
	// corridor offers included). Seats were only ever reserved on 'accepted'
	// rows, so refund those drivers' capacity — without this, bumped drivers
	// permanently lost a seat until they reset their route.
	// Capture the PRE-update status (RETURNING would show 'cancelled'):
	// lock, remember, then flip.
	rows, err := tx.Query(ctx, `
		SELECT id, driver_id, status FROM ride_requests
		WHERE passenger_id = $1
		  AND status IN ('offered','pending','accepted')
		  AND id <> $2
		FOR UPDATE
	`, passengerID, requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	type bumped struct{ ID, DriverID, WasStatus string }
	var bumpedRows []bumped
	for rows.Next() {
		var b bumped
		if rows.Scan(&b.ID, &b.DriverID, &b.WasStatus) == nil {
			bumpedRows = append(bumpedRows, b)
		}
	}
	rows.Close()
	if len(bumpedRows) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE ride_requests SET status = 'cancelled'
			WHERE passenger_id = $1
			  AND status IN ('offered','pending','accepted')
			  AND id <> $2
		`, passengerID, requestID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
	}
	for _, b := range bumpedRows {
		if b.WasStatus != models.RequestAccepted {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE active_drivers
			SET seats_taken = GREATEST(seats_taken - 1, 0),
			    status      = CASE WHEN status = 'full' THEN 'looking' ELSE status END
			WHERE driver_id = $1
		`, b.DriverID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
	}

	// 4b) Pickup-confirmation code, inside the transaction: an 'accepted' row
	// must never commit without a code (ConfirmPickup would reject every
	// attempt and the ride could never start).
	pickupCode, err := services.SetPickupCodeIn(ctx, tx, requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 5) Post-commit WS notifications.
	name, avg, count, carMake, carModel, carColor, carPlate, photoURL := loadDriverProfile(driverID)
	// Asymmetric pickup code: the passenger never sees the code (D1). They
	// must ask the driver verbally — that's the whole point of the handshake
	// as proof of physical meeting. The driver gets it in the JSON response
	// below (and via /driver/requests).
	Hub.SendToUser(passengerID, "driver_response", gin.H{
		"request_id":          requestID,
		"driver_id":           driverID,
		"driver_name":         name,
		"driver_photo_url":    photoURL,
		"driver_avg_rating":   avg,
		"driver_rating_count": count,
		"car_make":            carMake,
		"car_model":           carModel,
		"car_color":           carColor,
		"car_plate":           carPlate,
		"status":              "accepted",
		"driver_age":          userAge(ctx, driverID),
	})
	for _, b := range bumpedRows {
		// Drivers only ever saw pending/accepted rows; 'offered' rows lived
		// on the passenger's screen, so there is nothing to tear down there.
		if b.WasStatus == models.RequestOffered {
			continue
		}
		Hub.SendToUser(b.DriverID, "ride_cancelled", gin.H{
			"request_id":   b.ID,
			"cancelled_by": "passenger_matched_elsewhere",
		})
	}
	go safeGo("match-event", func() {
		logMatchEvent(requestID, "driver_accepted", nil)
		for _, b := range bumpedRows {
			logMatchEvent(b.ID, "cancelled", map[string]any{"by": "passenger_matched_elsewhere"})
		}
	})()

	c.JSON(http.StatusOK, gin.H{"ok": true, "pickup_code": pickupCode})
}

func loadDriverProfile(driverID string) (name string, avgRating float64, ratingCount int,
	carMake, carModel, carColor, carPlate, photoURL string) {
	db.DB.QueryRow(context.Background(), `
		SELECT u.name,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,''),
		       COALESCE(u.photo_url,''),
		       COALESCE(AVG(r.stars), 0), COUNT(r.id)
		FROM users u
		LEFT JOIN ratings r ON r.rated_id = u.id
		WHERE u.id = $1
		GROUP BY u.id, u.name, u.car_make, u.car_model, u.car_color, u.car_plate, u.photo_url
	`, driverID).Scan(&name, &carMake, &carModel, &carColor, &carPlate, &photoURL, &avgRating, &ratingCount)
	return
}

// userAge returns the user's age in years, or nil when no date of birth is
// on file. Only ever sent to the other party of an accepted ride.
func userAge(ctx context.Context, userID string) *int {
	var age *int
	_ = db.DB.QueryRow(ctx, `
		SELECT CASE WHEN birth_date IS NOT NULL
		            THEN DATE_PART('year', AGE(birth_date))::int END
		FROM users WHERE id = $1`, userID).Scan(&age)
	return age
}
