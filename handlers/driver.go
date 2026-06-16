package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
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
var (
	corridorRerunMu     sync.Mutex
	corridorRerunLastAt = map[string]time.Time{}
)

const corridorRerunInterval = 30 * time.Second

func shouldRerunCorridor(driverID string) bool {
	corridorRerunMu.Lock()
	defer corridorRerunMu.Unlock()
	now := time.Now()
	if last, ok := corridorRerunLastAt[driverID]; ok && now.Sub(last) < corridorRerunInterval {
		return false
	}
	corridorRerunLastAt[driverID] = now
	return true
}

// rerunCorridorMatch re-runs FindPassengersInCorridor with the driver's stored
// route + destination + corridor and notifies any newly-eligible passengers.
// Called from a goroutine in UpdateLocation; uses background context so a
// dropped HTTP connection doesn't cut the work short. Idempotent.
func rerunCorridorMatch(driverID string) {
	ctx := context.Background()
	var (
		routeWKT             string
		destLat, destLng     float64
		corridorKm           float64
		seats                int
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
	if err != nil || len(matches) == 0 {
		return
	}
	notifyCorridorMatches(driverID, matches, destLat, destLng, corridorKm, seats, false)
}

func SetRoute(c *gin.Context) {
	var req struct {
		OriginLat      float64 `json:"origin_lat" binding:"required"`
		OriginLng      float64 `json:"origin_lng" binding:"required"`
		DestinationLat float64 `json:"destination_lat" binding:"required"`
		DestinationLng float64 `json:"destination_lng" binding:"required"`
		CorridorKm     float64 `json:"corridor_km" binding:"required"`
		Seats          int     `json:"seats" binding:"required,min=1"`
		PreferAI       bool    `json:"prefer_ai"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	driverID := c.GetString("user_id")

	route, err := services.GetRoadRoute(req.OriginLat, req.OriginLng, req.DestinationLat, req.DestinationLng)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "routing failed: " + err.Error()})
		return
	}

	if err := services.UpsertActiveDriver(driverID, route.WKT,
		req.OriginLat, req.OriginLng, req.DestinationLat, req.DestinationLng,
		req.CorridorKm, req.Seats); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	matches, err := services.FindPassengersInCorridor(
		driverID, route.WKT, req.CorridorKm,
		req.DestinationLat, req.DestinationLng,
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
		req.DestinationLat, req.DestinationLng,
		req.CorridorKm, req.Seats, req.PreferAI,
	)

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
		driverName, carMake, carModel, carColor, carPlate, photoURL string
		driverAvgRating                                             float64
		driverRatingCount                                           int
	)
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
	`, driverID).Scan(&driverName, &carMake, &carModel, &carColor, &carPlate, &photoURL, &driverAvgRating, &driverRatingCount)

	ordered, methodTag := rankAndReorder(matches, driverID, corridorKm, seats, preferAI)

	notified := 0
	for _, m := range ordered {
		requestID, isNew, err := services.CreateRideRequest(driverID, m.User.ID, m.PickupLat, m.PickupLng)
		if err != nil || !isNew {
			continue
		}
		services.NotifyPassengerRideAvailable(m.User.FCMToken, driverID, destLat, destLng)
		Hub.SendToUser(m.User.ID, "ride_request", gin.H{
			"request_id":                  requestID,
			"driver_id":                   driverID,
			"driver_name":                 driverName,
			"driver_photo_url":            photoURL,
			"driver_avg_rating":           driverAvgRating,
			"driver_rating_count":         driverRatingCount,
			"car_make":                    carMake,
			"car_model":                   carModel,
			"car_color":                   carColor,
			"car_plate":                   carPlate,
			"destination_lat":             destLat,
			"destination_lng":             destLng,
			"corridor_km":                 corridorKm,
			"seats":                       seats,
			"pickup_lat":                  m.PickupLat,
			"pickup_lng":                  m.PickupLng,
			"walk_distance_m":             m.WalkM,
			"driver_distance_to_pickup_m": m.DriverToPickupM,
			"match_method":                methodTag,
			"match_kind":                  m.MatchKind,
		})
		notified++
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
	sortStable(ordered, func(a, b services.CorridorMatch) bool {
		ai, aok := posByID[a.User.ID]
		bi, bok := posByID[b.User.ID]
		switch {
		case aok && bok:
			return ai < bi
		case aok:
			return true
		case bok:
			return false
		}
		return false
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

// sortStable is a tiny stable bubble-sort-via-insertion wrapper so we avoid
// pulling sort.SliceStable's reflection cost in a hot path.
func sortStable[T any](xs []T, less func(a, b T) bool) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && less(xs[j], xs[j-1]); j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func UpdateLocation(c *gin.Context) {
	var req struct {
		Lat float64 `json:"lat" binding:"required"`
		Lng float64 `json:"lng" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	driverID := c.GetString("user_id")
	if db.Redis != nil {
		db.Redis.HSet(context.Background(), "driver:location:"+driverID, "lat", req.Lat, "lng", req.Lng)
	}

	// Mirror into PostGIS so the heading-aware corridor match can compute
	// passenger-is-ahead-of-driver. Best-effort — Redis remains the hot path.
	db.DB.Exec(c.Request.Context(), `
		UPDATE active_drivers
		SET current_location = ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
		WHERE driver_id = $3
	`, req.Lng, req.Lat, driverID)

	// As the driver moves, passengers who were previously "behind" or "out of
	// early-notify range" may become eligible. Re-run the corridor match —
	// idempotent CreateRideRequest means already-notified passengers are
	// skipped automatically. Throttled to one run per 30 s per driver.
	if shouldRerunCorridor(driverID) {
		go rerunCorridorMatch(driverID)
	}

	// Relay to any accepted passenger(s) over WS so the passenger's
	// _AcceptedCard mini-map sees the live driver position.
	rows, err := db.DB.Query(c.Request.Context(), `
		SELECT id, passenger_id FROM ride_requests
		WHERE driver_id = $1 AND status = 'accepted'
	`, driverID)
	if err == nil {
		for rows.Next() {
			var reqID, passengerID string
			if rows.Scan(&reqID, &passengerID) == nil {
				Hub.SendToUser(passengerID, "driver_location", gin.H{
					"request_id": reqID,
					"lat":        req.Lat,
					"lng":        req.Lng,
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
	driverID := c.GetString("user_id")
	if err := services.UpdateDriverStatus(driverID, req.Status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if req.Status == "done" {
		// "End driving day" — bulk-complete every remaining active ride
		// (accepted OR in_progress). Per-passenger drop-off uses
		// DropoffPassenger instead.
		rows, err := db.DB.Query(context.Background(), `
			SELECT id, passenger_id FROM ride_requests
			WHERE driver_id = $1 AND status IN ('accepted','in_progress')
		`, driverID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var reqID, passengerID string
				if rows.Scan(&reqID, &passengerID) == nil {
					if req.DistanceKm > 0 {
						services.RecordCompletedTrip(reqID, driverID, passengerID, req.DistanceKm)
					}
					payload := gin.H{
						"request_id":   reqID,
						"driver_id":    driverID,
						"passenger_id": passengerID,
					}
					Hub.SendToUser(passengerID, "ride_done", payload)
					Hub.SendToUser(driverID, "ride_done", payload)
				}
			}
			db.DB.Exec(context.Background(),
				`UPDATE ride_requests SET status = 'done'
				 WHERE driver_id = $1 AND status IN ('accepted','in_progress')`,
				driverID,
			)
			db.DB.Exec(context.Background(),
				`UPDATE active_drivers SET seats_taken = 0 WHERE driver_id = $1`,
				driverID,
			)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
	var passengerID, status string
	err = tx.QueryRow(ctx, `
		SELECT passenger_id, status FROM ride_requests
		WHERE id = $1 AND driver_id = $2
		FOR UPDATE
	`, requestID, driverID).Scan(&passengerID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride not found or not yours"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if status != "accepted" && status != "in_progress" {
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

	pickupWkt := ""
	pickupKm := 0.0
	if r, err := services.GetRoute("driving", driverLat, driverLng, pickupLat, pickupLng); err == nil {
		pickupWkt = r.WKT
		pickupKm = r.DistanceKm
	}

	continuationWkt := ""
	continuationKm := 0.0
	if r, err := services.GetRoute("driving", pickupLat, pickupLng, destLat, destLng); err == nil {
		continuationWkt = r.WKT
		continuationKm = r.DistanceKm
	}

	walkWkt := ""
	walkKm := 0.0
	if r, err := services.GetRoute("foot", passengerLat, passengerLng, pickupLat, pickupLng); err == nil {
		walkWkt = r.WKT
		walkKm = r.DistanceKm
	}

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

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func DeleteRoute(c *gin.Context) {
	driverID := c.GetString("user_id")

	rows, err := db.DB.Query(context.Background(),
		`UPDATE ride_requests
		 SET status = 'cancelled'
		 WHERE driver_id = $1 AND status IN ('pending','accepted')
		 RETURNING id, passenger_id`,
		driverID,
	)
	if err == nil {
		for rows.Next() {
			var reqID, passengerID string
			if rows.Scan(&reqID, &passengerID) == nil {
				Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
					"request_id":   reqID,
					"cancelled_by": "driver",
				})
			}
		}
		rows.Close()
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

	name, avg, count, carMake, carModel, carColor, carPlate, photoURL := loadDriverProfile(driverID)
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
		"status":              "declined",
	})

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

	// 4) Auto-cancel the PASSENGER's other pending/accepted rows.
	rows, err := tx.Query(ctx, `
		UPDATE ride_requests SET status = 'cancelled'
		WHERE passenger_id = $1
		  AND status IN ('pending','accepted')
		  AND id <> $2
		RETURNING id, driver_id
	`, passengerID, requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	type bumped struct{ ID, DriverID string }
	var bumpedRows []bumped
	for rows.Next() {
		var b bumped
		if rows.Scan(&b.ID, &b.DriverID) == nil {
			bumpedRows = append(bumpedRows, b)
		}
	}
	rows.Close()

	if err := tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// 5) Post-commit WS notifications.
	name, avg, count, carMake, carModel, carColor, carPlate, photoURL := loadDriverProfile(driverID)
	// Pickup-confirmation code: same as the passenger-side respondAccept path.
	pickupCode, _ := services.SetPickupCode(ctx, requestID)
	// Asymmetric pickup code: the passenger never sees the code (D1). They
	// must ask the driver verbally — that's the whole point of the handshake
	// as proof of physical meeting. The driver still gets the code below.
	_ = pickupCode
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
	})
	for _, b := range bumpedRows {
		Hub.SendToUser(b.DriverID, "ride_cancelled", gin.H{
			"request_id":   b.ID,
			"cancelled_by": "passenger_matched_elsewhere",
		})
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
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
