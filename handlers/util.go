package handlers

import (
	"log"
	"runtime/debug"
)

// validLatLng bounds-checks a WGS84 coordinate pair before it reaches
// PostGIS. Handlers bind lat/lng as *float64 (so 0 is a legal value) and
// then range-check here.
func validLatLng(lat, lng float64) bool {
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
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
