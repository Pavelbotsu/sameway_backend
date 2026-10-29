package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Fixed-window rate limiting, in-process.
//
// Deliberately the same shape as the existing throttles in this codebase
// (handlers.shouldRelayLiveLoc, handlers.allowPreRideMessage): a map guarded by
// a mutex, swept periodically from the reaper tick. That means it is per
// INSTANCE — with more than one backend replica each gets its own budget. Move
// to Redis if we ever scale out horizontally; until then a single instance
// makes this exact and free.
//
// Before this existed there was no rate limiting anywhere except the chat
// pre-ride cap, so login, registration, password-reset requests and the
// expensive geo endpoints were all unbounded.

type rateWindow struct {
	start time.Time
	count int
}

var (
	rateMu      sync.Mutex
	rateWindows = map[string]*rateWindow{}
)

// RateLimit allows `max` requests per `window` per caller for the route group
// identified by `name`.
//
// Callers are keyed by user id when the request is authenticated and by client
// IP otherwise, so one abusive account cannot spend everyone else's budget.
// Register it AFTER Auth() on authenticated routes — user_id is only in the
// context once Auth has run; otherwise it silently degrades to per-IP, which
// behind a reverse proxy means one shared bucket.
func RateLimit(name string, max int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowRate(name+":"+rateKey(c), max, window) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests,
				gin.H{"error": "too many requests — slow down"})
			return
		}
		c.Next()
	}
}

func rateKey(c *gin.Context) string {
	if uid := c.GetString("user_id"); uid != "" {
		return "u:" + uid
	}
	return "ip:" + c.ClientIP()
}

func allowRate(key string, max int, window time.Duration) bool {
	now := time.Now()
	rateMu.Lock()
	defer rateMu.Unlock()
	w, ok := rateWindows[key]
	if !ok || now.Sub(w.start) >= window {
		rateWindows[key] = &rateWindow{start: now, count: 1}
		return true
	}
	if w.count >= max {
		return false
	}
	w.count++
	return true
}

// SweepRateLimits drops windows that are long past. Called from the reaper tick
// alongside the other in-process sweeps; without it the map grows one entry per
// (route, caller) seen since boot.
func SweepRateLimits() {
	cutoff := time.Now().Add(-time.Hour)
	rateMu.Lock()
	for k, w := range rateWindows {
		if w.start.Before(cutoff) {
			delete(rateWindows, k)
		}
	}
	rateMu.Unlock()
}
