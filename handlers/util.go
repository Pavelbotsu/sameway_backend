package handlers

import (
	"log"
	"runtime/debug"
	"strings"
	"time"
)

// validLatLng bounds-checks a WGS84 coordinate pair before it reaches
// PostGIS. Handlers bind lat/lng as *float64 (so 0 is a legal value) and
// then range-check here.
func validLatLng(lat, lng float64) bool {
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

// firstName returns the first word of a display name. Before a ride is
// accepted other users see only this (see services.PrivacyGridDeg).
func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

// truncateRunes caps s at n CHARACTERS, never splitting a multi-byte one.
//
// Slicing a Go string by index slices bytes: with any non-ASCII input (this app
// is localized into Ukrainian, so that is the common case, not the edge case) a
// byte-index cut can land inside a character and yield invalid UTF-8. Postgres
// then rejects the whole INSERT.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// safeGo wraps a background task with panic recovery: `go safeGo(name, fn)()`.
// Gin recovers panics inside HTTP handlers, but a panic in a detached
// goroutine (corridor re-run, reaper sweep, FCM send) would crash the whole
// server and drop every WebSocket connection.
func safeGo(name string, fn func()) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic in %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}
}

// Background matching (corridor reruns, reverse trigger, micro-detour) is
// fire-and-forget and each run holds DB connections for heavy PostGIS work.
// Unbounded, a burst of location pings starved the pool and request handlers
// waited behind it (audit: location PUT p95 5.5 s at 1k passengers). At most
// bgMatchLimit runs execute at once; a run that can't start within
// bgMatchWait is dropped — every trigger is periodic, so the next ping
// retries.
const (
	bgMatchLimit = 4
	bgMatchWait  = 10 * time.Second
)

var bgMatchSem = make(chan struct{}, bgMatchLimit)

// goMatch runs fn in the background under the matching concurrency limit,
// with panic recovery.
func goMatch(name string, fn func()) {
	go safeGo(name, func() {
		runLimited(bgMatchSem, bgMatchWait, name, fn)
	})()
}

// runLimited runs fn once a slot in sem is free, or gives up after wait.
// Reports whether fn ran.
func runLimited(sem chan struct{}, wait time.Duration, name string, fn func()) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
	case <-timer.C:
		log.Printf("background %s skipped: %d matching runs already busy for %s", name, cap(sem), wait)
		return false
	}
	defer func() { <-sem }()
	fn()
	return true
}
