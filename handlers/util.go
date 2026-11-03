package handlers

import (
	"log"
	"runtime/debug"
	"strings"
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
