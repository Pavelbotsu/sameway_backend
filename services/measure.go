package services

import (
	"fmt"
	"math"
	"strings"
)

// assumedDriveMps is the fallback driving speed (≈30 km/h) used to synthesize
// per-segment travel times when OSRM didn't return duration annotations. Only
// affects the ETA measure when real durations are unavailable.
const assumedDriveMps = 8.3333

// BuildMeasuredLineEWKT builds an EWKT `LINESTRING M` from route coordinates
// (lng,lat pairs) where each vertex's M value is the cumulative travel time in
// seconds. Stored in active_drivers.route_m so ST_InterpolatePoint can return
// the drive-ETA to any projected point without a per-candidate OSRM call.
//
// durations is OSRM's per-segment seconds (len == len(coords)-1). When it's
// missing or mismatched, segment times are synthesized from great-circle
// distance / assumedDriveMps so route_m is never NULL. Returns "" for a
// degenerate (<2 point) line; callers store NULL in that case.
func BuildMeasuredLineEWKT(coords [][2]float64, durations []float64) string {
	if len(coords) < 2 {
		return ""
	}
	seg := durations
	if len(seg) != len(coords)-1 {
		seg = make([]float64, len(coords)-1)
		for i := 0; i < len(coords)-1; i++ {
			seg[i] = haversineM(coords[i], coords[i+1]) / assumedDriveMps
		}
	}
	parts := make([]string, len(coords))
	parts[0] = fmt.Sprintf("%f %f %f", coords[0][0], coords[0][1], 0.0)
	cum := 0.0
	for i := 1; i < len(coords); i++ {
		cum += seg[i-1]
		parts[i] = fmt.Sprintf("%f %f %f", coords[i][0], coords[i][1], cum)
	}
	return "SRID=4326;LINESTRING M(" + strings.Join(parts, ", ") + ")"
}

// HaversineM returns the great-circle distance in meters between two lat/lng
// points. Exported for callers that need a quick Go-side distance without a
// PostGIS round-trip (e.g. the distance-based corridor re-run throttle).
func HaversineM(lat1, lng1, lat2, lng2 float64) float64 {
	return haversineM([2]float64{lng1, lat1}, [2]float64{lng2, lat2})
}

// haversineM returns the great-circle distance in meters between two (lng,lat)
// points. Local helper so measure-building doesn't depend on PostGIS.
func haversineM(a, b [2]float64) float64 {
	const r = 6371000.0
	lat1 := a[1] * math.Pi / 180
	lat2 := b[1] * math.Pi / 180
	dLat := (b[1] - a[1]) * math.Pi / 180
	dLng := (b[0] - a[0]) * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}
