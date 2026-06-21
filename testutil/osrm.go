package testutil

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
)

// MockOSRM implements services.RoutingClient. Tests assign it to
// services.DefaultRoutingClient (see SwapRouter) for the duration of a test.
type MockOSRM struct {
	// Default is returned for any (origin, dest) when no specific entry hits.
	Default *services.RouteResult
}

// GetRoadRoute satisfies services.RoutingClient. Always returns Default if
// set; otherwise returns a deterministic straight-line route from origin to
// destination so any handler that needs a non-empty WKT gets one.
func (m *MockOSRM) GetRoadRoute(originLat, originLng, destLat, destLng float64) (*services.RouteResult, error) {
	return m.GetRoute("driving", originLat, originLng, destLat, destLng)
}

// GetRoute satisfies the profile-aware part of services.RoutingClient. The
// mock ignores the profile and returns a straight-line route — sufficient
// for handler tests that only assert "we got some WKT back".
func (m *MockOSRM) GetRoute(_ string, originLat, originLng, destLat, destLng float64) (*services.RouteResult, error) {
	if m.Default != nil {
		return m.Default, nil
	}
	wkt := LineStringWKT(
		[2]float64{originLng, originLat},
		[2]float64{destLng, destLat},
	)
	// Realistic single-segment metrics from the actual endpoints so the
	// measured route line / ETA gates behave like production: distance from
	// great-circle, duration at ~30 km/h (8.3333 m/s).
	distM := services.HaversineM(originLat, originLng, destLat, destLng)
	segS := distM / 8.3333
	return &services.RouteResult{
		WKT:        wkt,
		Coords:     [][2]float64{{originLng, originLat}, {destLng, destLat}},
		DistanceKm: distM / 1000,
		Durations:  []float64{segS},
		DurationS:  segS,
	}, nil
}

// SwapRouter installs m as services.DefaultRoutingClient for the test and
// restores the previous value on cleanup.
func SwapRouter(t testing.TB, m services.RoutingClient) {
	t.Helper()
	prev := services.DefaultRoutingClient
	services.DefaultRoutingClient = m
	t.Cleanup(func() { services.DefaultRoutingClient = prev })
}

// LineStringWKT builds a "LINESTRING(lng lat, …)" from (lng, lat) pairs. Use
// when a test wants a precise route geometry — e.g. heading-filter tests that
// need ST_LineLocatePoint to return predictable fractions.
func LineStringWKT(pts ...[2]float64) string {
	parts := make([]string, len(pts))
	for i, p := range pts {
		parts[i] = fmt.Sprintf("%f %f", p[0], p[1])
	}
	return "LINESTRING(" + strings.Join(parts, ", ") + ")"
}
