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
	"github.com/Pavelbotsu/sameway_backend/routes"
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
	r.Use(middleware.Logger(), gin.Recovery(), routes.BodyLimit())
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

	routes.Register(r, routes.Options{RateLimits: true, DevRoutes: !config.C.IsProd()})

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
