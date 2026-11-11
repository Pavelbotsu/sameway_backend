package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Live trip sharing: a rider sends a trusted contact a link that shows the
// car and where it is, until the ride ends (or at most shareMaxLife). The
// link is a random 128-bit token; anyone holding it sees only this ride, and
// only while it is going on.
const shareMaxLife = 6 * time.Hour

var shareTokenRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// CreateRideShare: POST /rides/:request_id/share. Either party of an
// accepted or in-progress ride. Responds {"url": "/s/<token>", "expires_at"}.
func CreateRideShare(c *gin.Context) {
	userID := c.GetString("user_id")
	rideID := c.Param("request_id")
	ctx := c.Request.Context()

	var driverID, passengerID, status string
	err := db.DB.QueryRow(ctx,
		`SELECT driver_id, passenger_id, status FROM ride_requests WHERE id = $1`, rideID,
	).Scan(&driverID, &passengerID, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && userID != driverID && userID != passengerID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if status != "accepted" && status != "in_progress" {
		c.JSON(http.StatusConflict, gin.H{"error": "only an ongoing ride can be shared", "code": "ride_not_active"})
		return
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	token := hex.EncodeToString(b)
	expires := time.Now().Add(shareMaxLife)
	if _, err := db.DB.Exec(ctx, `
		INSERT INTO ride_shares (token, ride_request_id, created_by, expires_at)
		VALUES ($1, $2, $3, $4)`, token, rideID, userID, expires); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": "/s/" + token, "expires_at": expires.UTC().Format(time.RFC3339)})
}

// RideSharePage: GET /s/:token — the public page. It is static; the live
// data comes from RideShareLive.
func RideSharePage(c *gin.Context) {
	if !shareTokenRe.MatchString(c.Param("token")) {
		c.Status(http.StatusNotFound)
		return
	}
	b, err := legalFS.ReadFile("static/share.html")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Robots-Tag", "noindex")
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// RideShareLive: GET /s/:token/live — JSON for the page. 410 once the ride
// is over or the link expired; 404 for unknown tokens.
func RideShareLive(c *gin.Context) {
	token := c.Param("token")
	c.Header("Cache-Control", "no-store")
	if !shareTokenRe.MatchString(token) {
		c.JSON(http.StatusNotFound, gin.H{"status": "unknown"})
		return
	}
	var (
		status, name, make_, model, color, plate string
		expires                                  time.Time
		pickLat, pickLng                         *float64
		carLat, carLng                           *float64
		carAt                                    *time.Time
	)
	err := db.DB.QueryRow(c.Request.Context(), `
		SELECT rr.status, s.expires_at, u.name,
		       COALESCE(u.car_make,''), COALESCE(u.car_model,''),
		       COALESCE(u.car_color,''), COALESCE(u.car_plate,''),
		       ST_Y(rr.pickup_location::geometry), ST_X(rr.pickup_location::geometry),
		       ST_Y(ad.current_location::geometry), ST_X(ad.current_location::geometry),
		       ad.current_location_at
		FROM ride_shares s
		JOIN ride_requests rr ON rr.id = s.ride_request_id
		JOIN users u ON u.id = rr.driver_id
		LEFT JOIN active_drivers ad ON ad.driver_id = rr.driver_id
		WHERE s.token = $1`, token,
	).Scan(&status, &expires, &name, &make_, &model, &color, &plate,
		&pickLat, &pickLng, &carLat, &carLng, &carAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"status": "unknown"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if time.Now().After(expires) || (status != "accepted" && status != "in_progress") {
		c.JSON(http.StatusGone, gin.H{"status": "ended"})
		return
	}
	out := gin.H{
		"status":      status,
		"driver_name": firstName(name),
		"car_make":    make_,
		"car_model":   model,
		"car_color":   color,
		"car_plate":   plate,
		"expires_at":  expires.UTC().Format(time.RFC3339),
	}
	if pickLat != nil && pickLng != nil {
		out["pickup"] = gin.H{"lat": *pickLat, "lng": *pickLng}
	}
	if carLat != nil && carLng != nil && carAt != nil {
		out["car"] = gin.H{"lat": *carLat, "lng": *carLng, "at": carAt.UTC().Format(time.RFC3339)}
	}
	c.JSON(http.StatusOK, out)
}
