package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

func Register(c *gin.Context) {
	var req struct {
		Name     string `json:"name" binding:"required"`
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=6"`
		Role     string `json:"role" binding:"required,oneof=driver passenger"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Normalize: the reset/verify flows lower-case emails before lookup, so a
	// mixed-case registration would otherwise never be able to reset its
	// password.
	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	var id string
	err = db.DB.QueryRow(context.Background(),
		`INSERT INTO users (name, email, password, role) VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Name, emailAddr, string(hash), req.Role,
	).Scan(&id)
	if err != nil {
		// Only a unique violation means "taken" — anything else is an outage
		// and must not masquerade as a duplicate email.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
			return
		}
		log.Printf("Register: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	token, err := middleware.GenerateToken(id, req.Role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"token": token, "user_id": id, "role": req.Role, "name": req.Name, "email": emailAddr})
}

func Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	emailAddr := strings.ToLower(strings.TrimSpace(req.Email))
	var id, hash, role, name string
	err := db.DB.QueryRow(context.Background(),
		`SELECT id, password, role, name FROM users WHERE email = $1`,
		emailAddr,
	).Scan(&id, &hash, &role, &name)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := middleware.GenerateToken(id, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user_id": id, "role": role, "name": name, "email": req.Email})
}

func GetProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	var u models.User
	err := db.DB.QueryRow(context.Background(), `
		SELECT id, name, email, role,
		       COALESCE(car_make,''), COALESCE(car_model,''),
		       COALESCE(car_color,''), COALESCE(car_plate,'')
		FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Name, &u.Email, &u.Role,
		&u.CarMake, &u.CarModel, &u.CarColor, &u.CarPlate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	var avgRating float64
	var ratingCount int
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(AVG(stars),0), COUNT(*) FROM ratings WHERE rated_id = $1`,
		userID,
	).Scan(&avgRating, &ratingCount)
	c.JSON(http.StatusOK, gin.H{
		"id":           u.ID,
		"name":         u.Name,
		"email":        u.Email,
		"role":         u.Role,
		"car_make":     u.CarMake,
		"car_model":    u.CarModel,
		"car_color":    u.CarColor,
		"car_plate":    u.CarPlate,
		"avg_rating":   avgRating,
		"rating_count": ratingCount,
	})
}

func GetTripHistory(c *gin.Context) {
	userID := c.GetString("user_id")
	rows, err := db.DB.Query(context.Background(), `
		SELECT id, driver_id, passenger_id, distance_km, co2_saved_kg, completed_at
		FROM completed_trips
		WHERE driver_id = $1 OR passenger_id = $1
		ORDER BY completed_at DESC
		LIMIT 50
	`, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	type tripItem struct {
		ID          string  `json:"id"`
		DriverID    string  `json:"driver_id"`
		PassengerID string  `json:"passenger_id"`
		DistanceKm  float64 `json:"distance_km"`
		CO2SavedKg  float64 `json:"co2_saved_kg"`
		CompletedAt string  `json:"completed_at"`
	}
	trips := []tripItem{}
	for rows.Next() {
		var t tripItem
		var completedAt time.Time
		if err := rows.Scan(&t.ID, &t.DriverID, &t.PassengerID, &t.DistanceKm, &t.CO2SavedKg, &completedAt); err == nil {
			t.CompletedAt = completedAt.UTC().Format(time.RFC3339)
			trips = append(trips, t)
		}
	}
	c.JSON(http.StatusOK, gin.H{"trips": trips, "user_id": userID})
}

// UpdateProfile is a partial update: only fields present in the JSON body are
// written. Pointer fields distinguish "absent" (keep) from "empty string"
// (deliberately clear) — previously a {"name": "..."}-only body silently
// wiped all car fields.
func UpdateProfile(c *gin.Context) {
	var req struct {
		Name     *string `json:"name"`
		CarMake  *string `json:"car_make"`
		CarModel *string `json:"car_model"`
		CarColor *string `json:"car_color"`
		CarPlate *string `json:"car_plate"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
		return
	}
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(context.Background(), `
		UPDATE users SET
		    name      = COALESCE($1, name),
		    car_make  = COALESCE($2, car_make),
		    car_model = COALESCE($3, car_model),
		    car_color = COALESCE($4, car_color),
		    car_plate = COALESCE($5, car_plate)
		WHERE id = $6`,
		req.Name, req.CarMake, req.CarModel, req.CarColor, req.CarPlate, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func SwitchRole(c *gin.Context) {
	var req struct {
		Role string `json:"role" binding:"required,oneof=driver passenger"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	oldRole := c.GetString("role")

	// Clean up any live state from the role the user is leaving so the new
	// role lands on a clean slate. Notify matched counterparties over WS so
	// their UI doesn't keep stale tiles. DB cleanup proceeds even if WS sends
	// fail (offline counterparty).
	ctx := c.Request.Context()
	switch oldRole {
	case "driver":
		// 'offered' included so no dangling corridor offers survive the
		// switch (a passenger could otherwise accept a ride from a driver
		// who no longer has a route).
		rows, err := db.DB.Query(ctx, `
			UPDATE ride_requests SET status = 'cancelled'
			WHERE driver_id = $1 AND status IN ('offered','pending','accepted')
			RETURNING id, passenger_id
		`, userID)
		if err == nil {
			for rows.Next() {
				var reqID, passengerID string
				if rows.Scan(&reqID, &passengerID) == nil {
					Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
						"request_id":   reqID,
						"cancelled_by": "driver_role_switch",
					})
				}
			}
			rows.Close()
		}
		db.DB.Exec(ctx, `DELETE FROM active_drivers WHERE driver_id = $1`, userID)

	case "passenger":
		// CTE so we report the PRE-update status (plain RETURNING would show
		// 'cancelled' for every row).
		rows, err := db.DB.Query(ctx, `
			WITH old AS (
				SELECT id, driver_id, status FROM ride_requests
				WHERE passenger_id = $1 AND status IN ('offered','pending','accepted')
			), upd AS (
				UPDATE ride_requests SET status = 'cancelled'
				WHERE id IN (SELECT id FROM old)
			)
			SELECT id, driver_id, status FROM old
		`, userID)
		if err == nil {
			for rows.Next() {
				var reqID, driverID, wasStatus string
				if rows.Scan(&reqID, &driverID, &wasStatus) == nil {
					// Drivers never saw 'offered' rows — nothing to tear down.
					if wasStatus == models.RequestOffered {
						continue
					}
					Hub.SendToUser(driverID, "passenger_request_cancelled", gin.H{
						"request_id":   reqID,
						"cancelled_by": "passenger_role_switch",
					})
				}
			}
			rows.Close()
		}
		db.DB.Exec(ctx, `DELETE FROM active_passengers WHERE passenger_id = $1`, userID)
	}

	token, err := middleware.GenerateToken(userID, req.Role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "role": req.Role, "user_id": userID})
}

func FCMStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"fcm_enabled": services.IsFCMReady()})
}

func TestNotification(c *gin.Context) {
	userID := c.GetString("user_id")
	var fcmToken string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`, userID,
	).Scan(&fcmToken)

	if err := services.SendTestNotification(fcmToken); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func ChangePassword(c *gin.Context) {
	var req struct {
		CurrentPassword string `json:"current_password" binding:"required"`
		NewPassword     string `json:"new_password" binding:"required,min=6"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.NewPassword == req.CurrentPassword {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new password must differ from current"})
		return
	}
	userID := c.GetString("user_id")
	var hash string
	err := db.DB.QueryRow(context.Background(),
		`SELECT password FROM users WHERE id = $1`, userID,
	).Scan(&hash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.CurrentPassword)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	_, err = db.DB.Exec(context.Background(),
		`UPDATE users SET password = $1 WHERE id = $2`, string(newHash), userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func GetStats(c *gin.Context) {
	userID := c.GetString("user_id")
	stats, err := services.GetUserStats(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func UpdateFCMToken(c *gin.Context) {
	var req struct {
		FCMToken string `json:"fcm_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(context.Background(),
		`UPDATE users SET fcm_token = $1 WHERE id = $2`,
		req.FCMToken, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
