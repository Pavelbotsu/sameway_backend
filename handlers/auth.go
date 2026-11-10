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
		// Both must be explicitly true: the 18+ attestation and acceptance of
		// the current Terms of Service + Privacy Policy. Recorded with a
		// timestamp and version so consent can be shown later.
		AcceptTerms  bool `json:"accept_terms"`
		AgeConfirmed bool `json:"age_confirmed"`
		// YYYY-MM-DD. 18+ is enforced on the date, not only on the checkbox.
		BirthDate string `json:"birth_date"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !req.AcceptTerms || !req.AgeConfirmed {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "you must be 18 or older and accept the terms to register",
			"code":  "consent_required",
		})
		return
	}
	birthDate, code, msg := parseBirthDate(req.BirthDate, time.Now())
	if code != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg, "code": code})
		return
	}
	if msg := validateRegistration(req.Name, req.Password); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	req.Name = strings.TrimSpace(req.Name)

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
		`INSERT INTO users (name, email, password, role,
		                    terms_version, terms_accepted_at, age_confirmed_at, birth_date)
		 VALUES ($1,$2,$3,$4,$5,NOW(),NOW(),$6) RETURNING id`,
		req.Name, emailAddr, string(hash), req.Role, CurrentTermsVersion, birthDate,
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
	c.JSON(http.StatusCreated, gin.H{"token": token, "user_id": id, "role": req.Role, "name": req.Name, "email": emailAddr, "needs_consent": false, "needs_birth_date": false})
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
	var suspended bool
	err := db.DB.QueryRow(context.Background(),
		`SELECT id, password, role, name, suspended_at IS NOT NULL
		 FROM users WHERE email = $1 AND deleted_at IS NULL`,
		emailAddr,
	).Scan(&id, &hash, &role, &name, &suspended)
	if err != nil {
		// Spend the same bcrypt time as a wrong password, so response time
		// doesn't reveal whether the email has an account.
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(req.Password))
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}
	// Checked only AFTER the password, so the suspension can't be used to
	// learn whether an address has an account.
	if suspended {
		c.JSON(http.StatusForbidden, gin.H{"error": "this account is suspended", "code": "account_suspended"})
		return
	}

	token, err := middleware.GenerateToken(id, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token": token, "user_id": id, "role": role, "name": name, "email": req.Email,
		// Accounts from before consent existed, or after a Terms version bump:
		// the app shows the acceptance screen before continuing.
		"needs_consent":    needsConsent(c.Request.Context(), id),
		"needs_birth_date": needsBirthDate(c.Request.Context(), id),
	})
}

func GetProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	var u models.User
	var allowMicroDetour, verified bool
	var photoURL, bio, phone string
	var emailVerifiedAt *time.Time
	err := db.DB.QueryRow(context.Background(), `
		SELECT id, name, email, role,
		       COALESCE(car_make,''), COALESCE(car_model,''),
		       COALESCE(car_color,''), COALESCE(car_plate,''),
		       allow_micro_detour,
		       COALESCE(photo_url,''), bio, phone, email_verified_at, verified
		FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Name, &u.Email, &u.Role,
		&u.CarMake, &u.CarModel, &u.CarColor, &u.CarPlate,
		&allowMicroDetour,
		&photoURL, &bio, &phone, &emailVerifiedAt, &verified)
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
		"id":                 u.ID,
		"name":               u.Name,
		"email":              u.Email,
		"role":               u.Role,
		"car_make":           u.CarMake,
		"car_model":          u.CarModel,
		"car_color":          u.CarColor,
		"car_plate":          u.CarPlate,
		"avg_rating":         avgRating,
		"rating_count":       ratingCount,
		"allow_micro_detour": allowMicroDetour,
		// Read by the Account & Security screen. Before these were returned
		// (and accepted by UpdateProfile) the screen saved them and silently
		// got nothing back.
		"photo_url":         photoURL,
		"bio":               bio,
		"phone":             phone,
		"email_verified_at": emailVerifiedAt,
		"verified":          verified,
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
		if err := rows.Scan(&t.ID, &t.DriverID, &t.PassengerID, &t.DistanceKm, &t.CO2SavedKg, &completedAt); err != nil {
			log.Printf("GetTripHistory: scan for %s: %v", userID, err)
			continue
		}
		t.CompletedAt = completedAt.UTC().Format(time.RFC3339)
		trips = append(trips, t)
	}
	// Without this a connection failure mid-stream returned a silently
	// truncated history as a 200 — the user sees "some of your trips vanished".
	if err := rows.Err(); err != nil {
		log.Printf("GetTripHistory: iteration for %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
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
		// Micro-detour opt-in (Round 2 §2) — server-side because it drives
		// matching, not just presentation.
		AllowMicroDetour *bool `json:"allow_micro_detour"`
		// Profile photo download URL from Firebase Storage; "" removes it.
		PhotoURL *string `json:"photo_url"`
		Bio      *string `json:"bio"`
		Phone    *string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
		return
	}
	if msg := validateProfileFields(req.Name, req.CarMake, req.CarModel, req.CarColor,
		req.CarPlate, req.PhotoURL, req.Bio, req.Phone); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if req.Bio != nil {
		b := strings.TrimSpace(*req.Bio)
		req.Bio = &b
	}
	if req.Phone != nil {
		p := strings.TrimSpace(*req.Phone)
		req.Phone = &p
	}
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(context.Background(), `
		UPDATE users SET
		    name      = COALESCE($1, name),
		    car_make  = COALESCE($2, car_make),
		    car_model = COALESCE($3, car_model),
		    car_color = COALESCE($4, car_color),
		    car_plate = COALESCE($5, car_plate),
		    allow_micro_detour = COALESCE($6, allow_micro_detour),
		    photo_url = CASE WHEN $8::text IS NULL THEN photo_url ELSE NULLIF($8::text, '') END,
		    bio       = COALESCE($9, bio),
		    phone     = COALESCE($10, phone)
		WHERE id = $7`,
		req.Name, req.CarMake, req.CarModel, req.CarColor, req.CarPlate,
		req.AllowMicroDetour, userID, req.PhotoURL, req.Bio, req.Phone,
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
				if err := rows.Scan(&reqID, &passengerID); err != nil {
					log.Printf("SwitchRole: scan cancelled ride for %s: %v", userID, err)
					continue
				}
				Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
					"request_id":   reqID,
					"cancelled_by": "driver_role_switch",
				})
			}
			if err := rows.Err(); err != nil {
				log.Printf("SwitchRole: iteration for %s (rides cancelled, notifications may be incomplete): %v", userID, err)
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
				if err := rows.Scan(&reqID, &driverID, &wasStatus); err != nil {
					log.Printf("SwitchRole: scan cancelled request for %s: %v", userID, err)
					continue
				}
				// Drivers never saw 'offered' rows — nothing to tear down.
				if wasStatus == models.RequestOffered {
					continue
				}
				Hub.SendToUser(driverID, "passenger_request_cancelled", gin.H{
					"request_id":   reqID,
					"cancelled_by": "passenger_role_switch",
				})
			}
			if err := rows.Err(); err != nil {
				log.Printf("SwitchRole: iteration for %s (requests cancelled, notifications may be incomplete): %v", userID, err)
			}
			rows.Close()
		}
		db.DB.Exec(ctx, `DELETE FROM active_passengers WHERE passenger_id = $1`, userID)
	}

	// Revoke the outgoing role's token BEFORE minting the new one (GenerateToken
	// stamps the current version). RequireRole trusts the role claim in the JWT,
	// so without this the pre-switch token stayed valid for its full lifetime
	// and the user could drive both role APIs at once — registering as an active
	// driver and an active passenger simultaneously.
	if err := middleware.BumpTokenVersion(ctx, db.DB, userID); err != nil {
		log.Printf("SwitchRole: bump token version for %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	token, err := middleware.GenerateToken(userID, req.Role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token": token, "role": req.Role, "user_id": userID,
		"needs_consent":    needsConsent(ctx, userID),
		"needs_birth_date": needsBirthDate(ctx, userID),
	})
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
	if len(req.NewPassword) > maxPasswordBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password is too long (max 72 bytes)"})
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
	// token_version bump evicts every other outstanding session. Changing your
	// password after a compromise has to actually log the attacker out; with
	// stateless JWTs that only happens if the version moves.
	_, err = db.DB.Exec(c.Request.Context(),
		`UPDATE users SET password = $1, token_version = token_version + 1 WHERE id = $2`,
		string(newHash), userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	// Open sockets were authorised with the old token; close them. Clients
	// reconnect with whatever token they hold, so a revoked session drops
	// out and this device reconnects with the fresh token returned below.
	Hub.Disconnect(userID)
	// The caller's own token is now stale too, so hand back a fresh one rather
	// than logging them out of the device they just used.
	token, err := middleware.GenerateToken(userID, c.GetString("role"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "token": token})
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
