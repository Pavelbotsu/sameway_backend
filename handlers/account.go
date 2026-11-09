package handlers

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Account deletion (Google Play "account deletion" policy, Apple 5.1.1(v),
// GDPR art. 17).
//
// The users row is SCRUBBED, not deleted. Hard-deleting it would cascade
// through ride_requests / completed_trips / ratings / user_reports and wipe the
// OTHER party's trip history, rating aggregates and any safety report filed
// against this user. Instead every personal field is overwritten, the login is
// made impossible, and personal content (chats, locations, planned trips, bug
// reports, blocks, rating comments) is deleted outright. What remains is an
// anonymous id referenced by other users' records — disclosed in the privacy
// policy.

var (
	errActiveRide    = errors.New("active ride")
	errAlreadyGone   = errors.New("account already deleted")
	errWrongPassword = errors.New("wrong password")
)

// deleteAccount scrubs userID inside one transaction and returns the WebSocket
// notifications to send once it has committed.
func deleteAccount(ctx context.Context, userID string) (notify []func(), err error) {
	tx, err := db.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Row lock serializes against a concurrent accept/role-switch for the same
	// user, so the active-ride check below can't be raced.
	var email string
	var deletedAt *string
	if err := tx.QueryRow(ctx,
		`SELECT email, deleted_at::text FROM users WHERE id = $1 FOR UPDATE`, userID,
	).Scan(&email, &deletedAt); err != nil {
		return nil, err
	}
	if deletedAt != nil {
		return nil, errAlreadyGone
	}

	// A ride someone is physically waiting for, or sitting in, must be ended
	// by the normal cancel/drop-off flow first — deleting underneath it would
	// strand the other person with no driver and no way to contact them.
	var active bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM ride_requests
			WHERE (driver_id = $1 OR passenger_id = $1)
			  AND status IN ('accepted','in_progress')
		)`, userID).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, errActiveRide
	}

	// Cancel open offers/requests and remember who has to hear about it.
	rows, err := tx.Query(ctx, `
		WITH old AS (
			SELECT id, driver_id, passenger_id, status FROM ride_requests
			WHERE (driver_id = $1 OR passenger_id = $1)
			  AND status IN ('offered','pending')
			FOR UPDATE
		), upd AS (
			UPDATE ride_requests SET status = 'cancelled'
			WHERE id IN (SELECT id FROM old)
		)
		SELECT id, driver_id, passenger_id, status FROM old`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var reqID, driverID, passengerID, status string
		if err := rows.Scan(&reqID, &driverID, &passengerID, &status); err != nil {
			rows.Close()
			return nil, err
		}
		switch {
		case driverID == userID:
			// The passenger may be looking at this offer or waiting on it.
			notify = append(notify, func() {
				Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
					"request_id": reqID, "cancelled_by": "driver_account_deleted",
				})
			})
		case status == models.RequestPending:
			// Drivers never see 'offered' rows, only pending ones.
			notify = append(notify, func() {
				Hub.SendToUser(driverID, "passenger_request_cancelled", gin.H{
					"request_id": reqID, "cancelled_by": "passenger_account_deleted",
				})
			})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, q := range []string{
		// Live location / presence.
		`DELETE FROM active_drivers WHERE driver_id = $1`,
		`DELETE FROM active_passengers WHERE passenger_id = $1`,
		// Planned trips carry free-text origin/destination names (addresses).
		`DELETE FROM planned_trips WHERE driver_id = $1`,
		// Chats: the whole conversation goes (messages cascade). Stray
		// ride-keyed messages with no conversation are removed explicitly.
		`DELETE FROM conversations WHERE user_a = $1 OR user_b = $1`,
		`DELETE FROM messages WHERE sender_id = $1`,
		`DELETE FROM user_blocks WHERE blocker_id = $1 OR blocked_id = $1`,
		`DELETE FROM bug_reports WHERE user_id = $1`,
		`DELETE FROM email_verifications WHERE user_id = $1`,
		// Free text this user wrote about others; the star value stays so the
		// other user's average doesn't silently change.
		`UPDATE ratings SET comment = NULL WHERE rater_id = $1`,
		// Pickup/drop-off points are the passenger's location history.
		`UPDATE ride_requests
		    SET pickup_location = NULL, dropoff_location = NULL, pickup_code = NULL
		  WHERE driver_id = $1 OR passenger_id = $1`,
		// Scrub the identity. password '!' is not a bcrypt hash, so every
		// comparison fails; the token_version bump kills existing sessions.
		`UPDATE users SET
		    name = 'Deleted user',
		    email = 'deleted-' || id::text || '@deleted.invalid',
		    password = '!',
		    fcm_token = NULL,
		    car_make = '', car_model = '', car_color = '', car_plate = '',
		    photo_url = NULL, bio = '', phone = '',
		    verified = FALSE, email_verified_at = NULL,
		    allow_micro_detour = FALSE,
		    token_version = token_version + 1,
		    deleted_at = NOW()
		  WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return nil, err
		}
	}
	// password_resets is keyed by email, not user id.
	if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE email = $1`, email); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	notify = append(notify, func() { Hub.Disconnect(userID) })
	return notify, nil
}

// checkPassword verifies the password of a live (not deleted) account.
func checkPassword(ctx context.Context, userID, password string) error {
	var hash string
	if err := db.DB.QueryRow(ctx,
		`SELECT password FROM users WHERE id = $1 AND deleted_at IS NULL`, userID,
	).Scan(&hash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errAlreadyGone
		}
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return errWrongPassword
	}
	return nil
}

func runDeletion(ctx context.Context, userID string) error {
	notify, err := deleteAccount(ctx, userID)
	if err != nil {
		return err
	}
	for _, n := range notify {
		n()
	}
	log.Printf("account deleted: %s", userID)
	return nil
}

// Logout handles POST /auth/logout. JWTs are stateless, so "logging out"
// used to mean only that the app forgot its token: the token kept working
// until it expired, and the device kept receiving the account's ride and chat
// pushes. Bumping token_version revokes every session of the account (the
// app re-logs in the device it's on), clearing fcm_token stops the pushes,
// and the live WebSocket is closed.
func Logout(c *gin.Context) {
	userID := c.GetString("user_id")
	if _, err := db.DB.Exec(c.Request.Context(), `
		UPDATE users SET token_version = token_version + 1, fcm_token = NULL
		WHERE id = $1`, userID); err != nil {
		log.Printf("Logout %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	Hub.Disconnect(userID)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteAccount handles POST /auth/delete-account (in-app). The password is
// re-checked so a stolen session token alone can't destroy the account.
// A wrong password is 403, not 401: the app treats any 401 on an
// authenticated call as an expired session and logs the user out.
func DeleteAccount(c *gin.Context) {
	var req struct {
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	ctx := c.Request.Context()

	if err := checkPassword(ctx, userID, req.Password); err != nil {
		switch {
		case errors.Is(err, errWrongPassword):
			c.JSON(http.StatusForbidden, gin.H{"error": "password is incorrect", "code": "wrong_password"})
		case errors.Is(err, errAlreadyGone):
			c.JSON(http.StatusGone, gin.H{"error": "account already deleted"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		}
		return
	}
	if err := runDeletion(ctx, userID); err != nil {
		respondDeletionError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func respondDeletionError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errActiveRide):
		c.JSON(http.StatusConflict, gin.H{
			"error": "finish or cancel your current ride before deleting your account",
			"code":  "active_ride",
		})
	case errors.Is(err, errAlreadyGone):
		c.JSON(http.StatusGone, gin.H{"error": "account already deleted"})
	default:
		log.Printf("delete account %s: %v", c.GetString("user_id"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
	}
}

// AccountDeletePage serves GET /account/delete — the public web deletion page
// Google Play requires alongside the in-app path (for users who no longer
// have the app installed).
var AccountDeletePage = serveStatic("delete_account.html")

// AccountDeleteWeb handles the form POST from AccountDeletePage. It never
// echoes user input back, and answers "wrong email or password" for both
// unknown emails and bad passwords so the form can't be used to probe which
// addresses have accounts.
func AccountDeleteWeb(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.PostForm("email")))
	password := c.PostForm("password")
	ctx := c.Request.Context()
	if email == "" || password == "" {
		deletionResultPage(c, http.StatusBadRequest, msgBadCredentials)
		return
	}
	var userID string
	if err := db.DB.QueryRow(ctx,
		`SELECT id FROM users WHERE email = $1 AND deleted_at IS NULL`, email,
	).Scan(&userID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			deletionResultPage(c, http.StatusInternalServerError, msgServerError)
			return
		}
		deletionResultPage(c, http.StatusUnauthorized, msgBadCredentials)
		return
	}
	if err := checkPassword(ctx, userID, password); err != nil {
		if errors.Is(err, errWrongPassword) || errors.Is(err, errAlreadyGone) {
			deletionResultPage(c, http.StatusUnauthorized, msgBadCredentials)
		} else {
			deletionResultPage(c, http.StatusInternalServerError, msgServerError)
		}
		return
	}
	if err := runDeletion(ctx, userID); err != nil {
		switch {
		case errors.Is(err, errActiveRide):
			deletionResultPage(c, http.StatusConflict, msgActiveRide)
		case errors.Is(err, errAlreadyGone):
			deletionResultPage(c, http.StatusOK, msgDeleted)
		default:
			log.Printf("web delete account %s: %v", userID, err)
			deletionResultPage(c, http.StatusInternalServerError, msgServerError)
		}
		return
	}
	deletionResultPage(c, http.StatusOK, msgDeleted)
}

// Fixed messages only (EN / PL / UK) — nothing user-supplied is rendered.
const (
	msgDeleted = `<h1>Account deleted</h1>
<p>Your Sameway account and personal data have been deleted.</p>
<p lang="pl">Twoje konto Sameway i dane osobowe zostały usunięte.</p>
<p lang="uk">Ваш обліковий запис Sameway і персональні дані видалено.</p>`
	msgBadCredentials = `<h1>Not deleted</h1>
<p>Wrong email or password.</p>
<p lang="pl">Nieprawidłowy e-mail lub hasło.</p>
<p lang="uk">Неправильна електронна пошта або пароль.</p>
<p><a href="/account/delete">Try again</a></p>`
	msgActiveRide = `<h1>Not deleted</h1>
<p>You have a ride in progress. Finish or cancel it in the app, then try again.</p>
<p lang="pl">Masz aktywny przejazd. Zakończ go lub anuluj w aplikacji i spróbuj ponownie.</p>
<p lang="uk">У вас є активна поїздка. Завершіть або скасуйте її в застосунку та спробуйте знову.</p>`
	msgServerError = `<h1>Something went wrong</h1>
<p>Please try again later.</p>
<p lang="pl">Spróbuj ponownie później.</p>
<p lang="uk">Спробуйте пізніше.</p>`
)

func deletionResultPage(c *gin.Context, status int, body string) {
	c.Header("Cache-Control", "no-store")
	c.Data(status, "text/html; charset=utf-8", []byte(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sameway — delete account</title>
<style>body{font-family:system-ui,sans-serif;max-width:560px;margin:40px auto;padding:0 16px;line-height:1.5}</style>
</head><body>`+body+`</body></html>`))
}
