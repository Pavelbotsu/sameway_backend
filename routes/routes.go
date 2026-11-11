// Package routes is the single HTTP route table, used by main.go and by
// testutil.NewTestRouter so tests exercise the same wiring as production.
package routes

import (
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/gin-gonic/gin"
)

// Options switch the parts that differ between production and tests.
type Options struct {
	// RateLimits applies the per-route limits. Off in most tests, which hit
	// the same endpoints far more often than any user would.
	RateLimits bool
	// DevRoutes registers diagnostics that leak deployment detail (FCM
	// readiness, AI model). Never in production.
	DevRoutes bool
}

// Register adds every route to r.
func Register(r *gin.Engine, o Options) {
	rl := func(name string, max int, window time.Duration) gin.HandlerFunc {
		if !o.RateLimits {
			return func(c *gin.Context) { c.Next() }
		}
		return middleware.RateLimit(name, max, window)
	}

	// Liveness/readiness probe for the container healthcheck. Unauthenticated
	// by design; reports DB reachability.
	r.GET("/healthz", func(c *gin.Context) {
		if err := db.DB.Ping(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"ok": false})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	auth := r.Group("/auth")
	{
		// Unauthenticated and credential-bearing: keyed by IP, since there is
		// no user id yet. Bcrypt makes each attempt costly server-side, so the
		// cap protects the CPU as much as the accounts.
		auth.POST("/register", rl("register", 5, time.Minute), handlers.Register)
		auth.POST("/login", rl("login", 10, time.Minute), handlers.Login)
		auth.POST("/forgot-password", rl("forgot", 5, time.Minute), handlers.ForgotPassword)
		auth.POST("/reset-password", rl("reset", 10, time.Minute), handlers.ResetPassword)
		auth.GET("/profile", middleware.Auth(), handlers.GetProfile)
		auth.PUT("/profile", middleware.Auth(),
			rl("profile", 20, time.Minute), handlers.UpdateProfile)
		// Stored on our server, metadata stripped (no Firebase Storage).
		auth.POST("/profile/photo", middleware.Auth(),
			rl("photo", 10, time.Minute), handlers.UploadProfilePhoto)
		auth.POST("/switch-role", middleware.Auth(), handlers.SwitchRole)
		auth.PUT("/fcm-token", middleware.Auth(), handlers.UpdateFCMToken)
		auth.GET("/stats", middleware.Auth(), handlers.GetStats)
		auth.POST("/change-password", middleware.Auth(), handlers.ChangePassword)
		auth.GET("/trips", middleware.Auth(), handlers.GetTripHistory)
		auth.POST("/bug-report", middleware.Auth(), handlers.SubmitBugReport)
		auth.POST("/email-verify/request", middleware.Auth(), handlers.RequestEmailVerify)
		auth.POST("/email-verify/confirm", middleware.Auth(), handlers.ConfirmEmailVerify)
		// Terms/Privacy acceptance + 18+ attestation for accounts that predate
		// it (or after a CurrentTermsVersion bump).
		auth.POST("/consent", middleware.Auth(), handlers.AcceptTerms)
		// Licence / insurance / roadworthy car / cost-sharing, required before
		// a driver can publish a route or a planned trip.
		auth.POST("/driver-declaration", middleware.Auth(), handlers.AcceptDriverDeclaration)
		// In-app account deletion (Play + App Store requirement). Password is
		// re-checked; the limit just stops a stolen token being used to
		// brute-force it.
		// Revokes this and every other session of the account, and unbinds the
		// push token so the device stops getting the account's notifications.
		auth.POST("/logout", middleware.Auth(), handlers.Logout)
		// Copy of everything held about the account (GDPR access/portability).
		auth.GET("/export", middleware.Auth(),
			rl("export", 5, time.Minute), handlers.ExportData)
		auth.POST("/delete-account", middleware.Auth(),
			rl("delete-account", 5, time.Minute), handlers.DeleteAccount)

		// Diagnostics — dev only. These exist to debug an install (is FCM
		// wired? does the AI provider answer?) and leak deployment detail
		// (configured model name, FCM readiness) that production has no reason
		// to publish.
		if o.DevRoutes {
			auth.GET("/fcm-status", middleware.Auth(), handlers.FCMStatus)
			auth.POST("/test-notification", middleware.Auth(), handlers.TestNotification)
			auth.POST("/test-ai", middleware.Auth(), handlers.TestAIRanker)
		}
	}

	r.POST("/ratings", middleware.Auth(), middleware.RequireConsent(),
		rl("ratings", 10, time.Minute), handlers.SubmitRating)
	// Second line of defence behind the per-ride pickup_attempts cap: stops a
	// script hammering codes across many rides at once.
	r.POST("/pickup/confirm", middleware.Auth(), middleware.RequireConsent(),
		rl("pickup", 10, time.Minute), handlers.ConfirmPickup)

	// Pickup route — accessible to both driver and passenger of the ride.
	// The handler enforces participant-of-the-row.
	r.GET("/pickup-route/:request_id", middleware.Auth(), middleware.RequireConsent(), handlers.GetPickupRoute)

	// Mid-ride safety check-in flag. Either party may raise it, so it sits here
	// rather than in a role group; the handler enforces participant-of-the-row.
	// The limit is generous relative to the one prompt per ride the client can
	// show, and only exists to stop a script stamping every ride it can name.
	r.POST("/safety/flag", middleware.Auth(),
		rl("safety", 5, time.Minute), handlers.FlagRideSafety)

	driver := r.Group("/driver", middleware.Auth(), middleware.RequireConsent(), middleware.RequireRole("driver"))
	{
		// Each call = an OSRM route + the heaviest matching query + up to 3
		// via-routes for micro-detour; a few per minute is generous.
		driver.POST("/route", rl("set-route", 10, time.Minute), handlers.SetRoute)
		driver.POST("/respond", handlers.DriverRespondToRide)
		driver.PUT("/location", handlers.UpdateLocation)
		driver.GET("/requests", handlers.GetRequests)
		driver.PUT("/status", handlers.UpdateStatus)
		driver.POST("/dropoff/:request_id", handlers.DropoffPassenger)
		driver.DELETE("/route", handlers.DeleteRoute)
		driver.DELETE("/ride/:request_id", handlers.DriverCancelRide)
		// Way out of a pickup-code lockout: a new code, attempts reset.
		driver.POST("/pickup-code/:request_id/regenerate",
			rl("pickup-regen", 10, time.Minute), handlers.RegeneratePickupCode)
		// Passenger never showed: allowed after MATCH_NO_SHOW_WAIT_S.
		driver.POST("/no-show/:request_id", handlers.ReportNoShow)
		driver.POST("/trips", rl("plan-trip", 10, time.Minute), handlers.PlanTrip)
		driver.GET("/trips", handlers.GetMyPlannedTrips)
		driver.DELETE("/trips/:id", handlers.CancelPlannedTrip)
	}

	passenger := r.Group("/passenger", middleware.Auth(), middleware.RequireConsent(), middleware.RequireRole("passenger"))
	{
		passenger.PUT("/location", handlers.PassengerUpdateLocation)
		passenger.POST("/respond", handlers.RespondToRide)
		// Heavy PostGIS per call and polled by the client every 10s; the cap
		// leaves normal polling untouched while stopping a scripted city sweep.
		passenger.POST("/nearby-drivers",
			rl("nearby", 20, time.Minute), handlers.NearbyDrivers)
		passenger.POST("/search", handlers.PassengerSearch)
		// Each request pushes a notification to the driver.
		passenger.POST("/request-driver",
			rl("request-driver", 10, time.Minute), handlers.RequestDriver)
		// The costliest query in the codebase (101-step generate_series over
		// geodesic ops) — never leave it unbounded.
		passenger.POST("/reposition",
			rl("reposition", 10, time.Minute), handlers.RepositionForDriver)
		passenger.POST("/match-advice", handlers.MatchAdvice)
		passenger.GET("/my-requests", handlers.GetMyRequests)
		passenger.DELETE("/active", handlers.GoOffline)
		passenger.DELETE("/ride/:request_id", handlers.PassengerCancelRide)
		passenger.DELETE("/request/:request_id", handlers.PassengerCancelPendingRequest)
	}

	chat := r.Group("/chat", middleware.Auth(), middleware.RequireConsent())
	{
		// Conversation-first API (Round 2 §3): persistent pair threads.
		chat.POST("/conversations", handlers.OpenConversation)
		chat.GET("/conversations", handlers.ListConversations)
		chat.GET("/conversations/:id/messages", handlers.GetConversationMessages)
		// Overall ceiling; pairs without a completed ride also have the
		// tighter per-conversation pre-ride limit inside the handler.
		chat.POST("/send", rl("chat-send", 30, time.Minute), handlers.SendMessage)
		// Typing is a pure WS amplifier — one cheap POST pushes an event to the
		// peer, so it needs its own ceiling independent of the message limit.
		chat.POST("/typing", rl("typing", 30, time.Minute), handlers.TypingStatus)
		chat.POST("/read", handlers.MarkRead)
		chat.POST("/block", handlers.BlockUser)
		chat.DELETE("/block/:user_id", handlers.UnblockUser)
		chat.POST("/report", rl("report", 10, time.Minute), handlers.ReportUser)
		// Compat shim (one release): ride-scoped history for old clients.
		chat.GET("/:request_id", handlers.GetMessages)
	}

	// Auth required: the response carries driver identity, car description, and
	// trip endpoints. Unauthenticated, this was a city-wide harvest of every
	// driver's name, car, departure point and time.
	r.POST("/trips/search", middleware.Auth(), middleware.RequireConsent(),
		rl("trip-search", 30, time.Minute), handlers.SearchTrips)
	r.GET("/promotions", handlers.GetPromotions)

	// Public legal pages and the web account-deletion form. Their URLs go in
	// the Play Console (privacy policy + "delete account" link) and App Store
	// Connect, and the app opens them from signup and settings.
	r.GET("/legal/privacy", handlers.PrivacyPolicy)
	r.GET("/legal/terms", handlers.TermsOfService)
	r.GET("/legal/style.css", handlers.LegalStyle)
	r.GET("/account/delete", handlers.AccountDeletePage)
	// Unauthenticated and password-checking: per-IP, like /auth/login.
	r.POST("/account/delete", rl("web-delete", 5, time.Minute), handlers.AccountDeleteWeb)

	r.GET("/photos/:file", handlers.ServePhoto)
	r.GET("/ws", handlers.HandleWS)
}

// BodyLimit is the request-size cap used by the server and the test router:
// 64 KB for JSON APIs, more for the profile photo upload.
func BodyLimit() gin.HandlerFunc {
	return middleware.BodyLimit(64<<10, map[string]int64{
		"/auth/profile/photo": 3<<20 + 64<<10, // 3 MB image + multipart overhead
	})
}
