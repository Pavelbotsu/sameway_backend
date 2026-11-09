package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/services/email"
	"github.com/Pavelbotsu/sameway_backend/services/ranker"
	"github.com/gin-gonic/gin"
)

func main() {
	config.Load()
	if err := config.C.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	// Release mode unconditionally. Debug mode dumps the whole route table on
	// boot and enables Gin's debug warnings; there is no deployment of this
	// server that wants that, and gating it on IsProd meant a compose file
	// setting APP_ENV=dev silently ran a production host in debug.
	gin.SetMode(gin.ReleaseMode)

	if err := db.Connect(); err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.DB.Close()

	services.InitFirebase()
	ranker.Init(config.C)
	email.Init(config.C)
	// Transit stops for partial-relay feasibility — background refresh, boot
	// never blocks on the download.
	services.StartGTFSLoader()

	// gin.New() + explicit middleware rather than gin.Default(): the stock
	// logger writes the raw query string, which would put the WebSocket
	// handshake's `?token=<jwt>` into the log on every connect.
	r := gin.New()
	// 64 KB is far above any legitimate JSON body here; without a cap a single
	// request could stream an unbounded body into the JSON decoder.
	r.Use(middleware.Logger(), gin.Recovery(), middleware.BodyLimit(64<<10))
	// TLS terminates at the reverse proxy (Caddy), which is the ONLY thing that
	// can reach this container — docker-compose.prod.yml publishes no ports for
	// the app service. Trusting the private ranges therefore trusts exactly that
	// one hop, and lets c.ClientIP() resolve the real caller from
	// X-Forwarded-For. With the previous nil (trust nothing), every request
	// looked like it came from Caddy's container IP, which would have collapsed
	// the per-IP rate limiter into a single shared bucket for all users.
	if err := r.SetTrustedProxies([]string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.1/32",
	}); err != nil {
		log.Fatalf("trusted proxies: %v", err)
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
		auth.POST("/register", middleware.RateLimit("register", 5, time.Minute), handlers.Register)
		auth.POST("/login", middleware.RateLimit("login", 10, time.Minute), handlers.Login)
		auth.POST("/forgot-password", middleware.RateLimit("forgot", 5, time.Minute), handlers.ForgotPassword)
		auth.POST("/reset-password", middleware.RateLimit("reset", 10, time.Minute), handlers.ResetPassword)
		auth.GET("/profile", middleware.Auth(), handlers.GetProfile)
		auth.PUT("/profile", middleware.Auth(),
			middleware.RateLimit("profile", 20, time.Minute), handlers.UpdateProfile)
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
		// In-app account deletion (Play + App Store requirement). Password is
		// re-checked; the limit just stops a stolen token being used to
		// brute-force it.
		// Revokes this and every other session of the account, and unbinds the
		// push token so the device stops getting the account's notifications.
		auth.POST("/logout", middleware.Auth(), handlers.Logout)
		// Copy of everything held about the account (GDPR access/portability).
		auth.GET("/export", middleware.Auth(),
			middleware.RateLimit("export", 5, time.Minute), handlers.ExportData)
		auth.POST("/delete-account", middleware.Auth(),
			middleware.RateLimit("delete-account", 5, time.Minute), handlers.DeleteAccount)

		// Diagnostics — dev only. These exist to debug an install (is FCM
		// wired? does the AI provider answer?) and leak deployment detail
		// (configured model name, FCM readiness) that production has no reason
		// to publish.
		if !config.C.IsProd() {
			auth.GET("/fcm-status", middleware.Auth(), handlers.FCMStatus)
			auth.POST("/test-notification", middleware.Auth(), handlers.TestNotification)
			auth.POST("/test-ai", middleware.Auth(), handlers.TestAIRanker)
		}
	}

	r.POST("/ratings", middleware.Auth(), middleware.RequireConsent(),
		middleware.RateLimit("ratings", 10, time.Minute), handlers.SubmitRating)
	// Second line of defence behind the per-ride pickup_attempts cap: stops a
	// script hammering codes across many rides at once.
	r.POST("/pickup/confirm", middleware.Auth(), middleware.RequireConsent(),
		middleware.RateLimit("pickup", 10, time.Minute), handlers.ConfirmPickup)

	// Pickup route — accessible to both driver and passenger of the ride.
	// The handler enforces participant-of-the-row.
	r.GET("/pickup-route/:request_id", middleware.Auth(), middleware.RequireConsent(), handlers.GetPickupRoute)

	// Mid-ride safety check-in flag. Either party may raise it, so it sits here
	// rather than in a role group; the handler enforces participant-of-the-row.
	// The limit is generous relative to the one prompt per ride the client can
	// show, and only exists to stop a script stamping every ride it can name.
	r.POST("/safety/flag", middleware.Auth(),
		middleware.RateLimit("safety", 5, time.Minute), handlers.FlagRideSafety)

	driver := r.Group("/driver", middleware.Auth(), middleware.RequireConsent(), middleware.RequireRole("driver"))
	{
		// Each call = an OSRM route + the heaviest matching query + up to 3
		// via-routes for micro-detour; a few per minute is generous.
		driver.POST("/route", middleware.RateLimit("set-route", 10, time.Minute), handlers.SetRoute)
		driver.POST("/respond", handlers.DriverRespondToRide)
		driver.PUT("/location", handlers.UpdateLocation)
		driver.GET("/requests", handlers.GetRequests)
		driver.PUT("/status", handlers.UpdateStatus)
		driver.POST("/dropoff/:request_id", handlers.DropoffPassenger)
		driver.DELETE("/route", handlers.DeleteRoute)
		driver.DELETE("/ride/:request_id", handlers.DriverCancelRide)
		// Way out of a pickup-code lockout: a new code, attempts reset.
		driver.POST("/pickup-code/:request_id/regenerate",
			middleware.RateLimit("pickup-regen", 10, time.Minute), handlers.RegeneratePickupCode)
		// Passenger never showed: allowed after MATCH_NO_SHOW_WAIT_S.
		driver.POST("/no-show/:request_id", handlers.ReportNoShow)
		driver.POST("/trips", middleware.RateLimit("plan-trip", 10, time.Minute), handlers.PlanTrip)
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
			middleware.RateLimit("nearby", 20, time.Minute), handlers.NearbyDrivers)
		passenger.POST("/search", handlers.PassengerSearch)
		// Each request pushes a notification to the driver.
		passenger.POST("/request-driver",
			middleware.RateLimit("request-driver", 10, time.Minute), handlers.RequestDriver)
		// The costliest query in the codebase (101-step generate_series over
		// geodesic ops) — never leave it unbounded.
		passenger.POST("/reposition",
			middleware.RateLimit("reposition", 10, time.Minute), handlers.RepositionForDriver)
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
		chat.POST("/send", middleware.RateLimit("chat-send", 30, time.Minute), handlers.SendMessage)
		// Typing is a pure WS amplifier — one cheap POST pushes an event to the
		// peer, so it needs its own ceiling independent of the message limit.
		chat.POST("/typing", middleware.RateLimit("typing", 30, time.Minute), handlers.TypingStatus)
		chat.POST("/read", handlers.MarkRead)
		chat.POST("/block", handlers.BlockUser)
		chat.DELETE("/block/:user_id", handlers.UnblockUser)
		chat.POST("/report", middleware.RateLimit("report", 10, time.Minute), handlers.ReportUser)
		// Compat shim (one release): ride-scoped history for old clients.
		chat.GET("/:request_id", handlers.GetMessages)
	}

	// Auth required: the response carries driver identity, car description, and
	// trip endpoints. Unauthenticated, this was a city-wide harvest of every
	// driver's name, car, departure point and time.
	r.POST("/trips/search", middleware.Auth(), middleware.RequireConsent(),
		middleware.RateLimit("trip-search", 30, time.Minute), handlers.SearchTrips)
	r.GET("/promotions", handlers.GetPromotions)

	// Public legal pages and the web account-deletion form. Their URLs go in
	// the Play Console (privacy policy + "delete account" link) and App Store
	// Connect, and the app opens them from signup and settings.
	r.GET("/legal/privacy", handlers.PrivacyPolicy)
	r.GET("/legal/terms", handlers.TermsOfService)
	r.GET("/account/delete", handlers.AccountDeletePage)
	// Unauthenticated and password-checking: per-IP, like /auth/login.
	r.POST("/account/delete", middleware.RateLimit("web-delete", 5, time.Minute), handlers.AccountDeleteWeb)

	r.GET("/ws", handlers.HandleWS)

	go handlers.ReapPendingRideRequests(config.C.RideRequestTimeoutMinutes)

	// Graceful shutdown. gin.Run() installs no signal handling, so a redeploy
	// (`docker compose up -d --build` → SIGTERM) killed the process instantly:
	// in-flight requests were severed mid-transaction, every WebSocket dropped
	// without a close frame — so clients saw a hard error rather than a clean
	// reconnect — and the deferred db.Close never ran.
	srv := &http.Server{
		Addr:    ":" + config.C.Port,
		Handler: r,
		// No WriteTimeout: WebSocket connections live on these sockets and any
		// write deadline would kill them. The WS layer has its own
		// wsWriteWait/wsPongWait keepalive, and the reverse proxy bounds
		// ordinary requests.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Listening on :%s", config.C.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	stop() // restore default handling: a second Ctrl-C/SIGTERM kills immediately
	log.Println("shutting down: draining connections (15s)")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Tell WS clients to reconnect before the socket vanishes underneath them,
	// otherwise Shutdown just waits out the timeout on those long-lived conns.
	handlers.Hub.CloseAll()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("stopped")
}
