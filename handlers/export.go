package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// ExportData handles GET /auth/export: a copy of the personal data held about
// the caller, as one JSON document (GDPR art. 15 access, art. 20
// portability). Data about other people is limited to their ids — e.g. the
// rating stars someone gave you, not who they are beyond the ride.
func ExportData(c *gin.Context) {
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	sections := []struct {
		name, sql string
	}{
		{"profile", `
			SELECT id, name, email, role, phone, bio, car_make, car_model,
			       car_color, car_plate, photo_url, created_at, email_verified_at,
			       terms_version, terms_accepted_at, age_confirmed_at,
			       birth_date, allow_micro_detour
			FROM users WHERE id = $1`},
		{"rides", `
			SELECT id,
			       CASE WHEN driver_id = $1 THEN 'driver' ELSE 'passenger' END AS your_role,
			       CASE WHEN driver_id = $1 THEN passenger_id ELSE driver_id END AS other_user_id,
			       status, match_kind, created_at, picked_up_at, cancel_reason,
			       ST_Y(pickup_location::geometry) AS pickup_lat,
			       ST_X(pickup_location::geometry) AS pickup_lng
			FROM ride_requests
			WHERE driver_id = $1 OR passenger_id = $1
			ORDER BY created_at`},
		{"completed_trips", `
			SELECT ride_request_id, distance_km, co2_saved_kg, completed_at
			FROM completed_trips
			WHERE driver_id = $1 OR passenger_id = $1
			ORDER BY completed_at`},
		{"ratings_given", `
			SELECT ride_request_id, rated_id AS rated_user_id, stars, comment, created_at
			FROM ratings WHERE rater_id = $1 ORDER BY created_at`},
		{"ratings_received", `
			SELECT ride_request_id, stars, comment, created_at
			FROM ratings WHERE rated_id = $1 ORDER BY created_at`},
		{"messages_sent", `
			SELECT conversation_id, content, created_at, read_at
			FROM messages WHERE sender_id = $1 ORDER BY created_at`},
		{"planned_trips", `
			SELECT origin_name, destination_name, planned_at, seats, status, created_at
			FROM planned_trips WHERE driver_id = $1 ORDER BY created_at`},
		{"reports_filed", `
			SELECT reported_id AS reported_user_id, reason, created_at
			FROM user_reports WHERE reporter_id = $1 ORDER BY created_at`},
		{"blocked_users", `
			SELECT blocked_id AS user_id, created_at
			FROM user_blocks WHERE blocker_id = $1 ORDER BY created_at`},
		{"bug_reports", `
			SELECT title, body, app_version, device_info, created_at
			FROM bug_reports WHERE user_id = $1 ORDER BY created_at`},
	}

	out := gin.H{
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"note": "Personal data Sameway holds about you. Other people appear " +
			"only as ids. Questions: see the privacy policy.",
	}
	for _, s := range sections {
		rows, err := queryMaps(ctx, s.sql, userID)
		if err != nil {
			log.Printf("ExportData %s/%s: %v", userID, s.name, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
			return
		}
		if s.name == "profile" {
			if len(rows) == 1 {
				out["profile"] = rows[0]
			}
			continue
		}
		out[s.name] = rows
	}

	c.Header("Content-Disposition", fmt.Sprintf(
		`attachment; filename="sameway-data-%s.json"`, time.Now().UTC().Format("2006-01-02")))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, out)
}

// queryMaps runs sql with args and returns each row as column → value.
// UUID columns arrive as raw [16]byte; they're turned into the usual text
// form so the export is readable.
func queryMaps(ctx context.Context, sql string, args ...any) ([]map[string]any, error) {
	rows, err := db.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		return nil, err
	}
	for _, row := range out {
		for k, v := range row {
			if b, ok := v.([16]byte); ok {
				row[k] = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
			}
		}
	}
	return out, nil
}
