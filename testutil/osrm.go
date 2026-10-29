package testutil

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
)

// MockOSRM implements services.RoutingClient. Tests assign it to
// services.DefaultRoutingClient (see SwapRouter) for the duration of a test.
type MockOSRM struct {
	// Default is returned for any (origin, dest) when no specific entry hits.
	Default *services.RouteResult
	// ViaResult, when set, is returned by every GetRouteVia call with 3+
	// points (micro-detour evaluations). Two-point via calls stay on the
	// straight-line path so baselines behave like GetRoute.
	ViaResult *services.RouteResult

	mu sync.Mutex
	// RouteCalls records every 2-point request as {oLat, oLng, dLat, dLng}.
	RouteCalls [][4]float64
	// ViaCalls records every 3+-point request's waypoints ((lat,lng) pairs).
	ViaCalls [][][2]float64
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
	m.mu.Lock()
	m.RouteCalls = append(m.RouteCalls, [4]float64{originLat, originLng, destLat, destLng})
	m.mu.Unlock()
	if m.Default != nil {
		return m.Default, nil
	}
	return m.straightLine(originLat, originLng, destLat, destLng), nil
}

// GetRouteVia records the waypoints and returns ViaResult for genuine
// via-point requests, else a straight leg-sum route.
func (m *MockOSRM) GetRouteVia(_ string, points ...[2]float64) (*services.RouteResult, error) {
	if len(points) < 2 {
		return nil, fmt.Errorf("mock osrm: need 2+ waypoints")
	}
	if len(points) == 2 {
		return m.GetRoute("driving", points[0][0], points[0][1], points[1][0], points[1][1])
	}
	m.mu.Lock()
	m.ViaCalls = append(m.ViaCalls, points)
	m.mu.Unlock()
	if m.ViaResult != nil {
		return m.ViaResult, nil
	}
	var distM float64
	for i := 1; i < len(points); i++ {
		distM += services.HaversineM(points[i-1][0], points[i-1][1], points[i][0], points[i][1])
	}
	segS := distM / 8.3333
	return &services.RouteResult{
		WKT:        LineStringWKT([2]float64{points[0][1], points[0][0]}, [2]float64{points[len(points)-1][1], points[len(points)-1][0]}),
		DistanceKm: distM / 1000,
		DurationS:  segS,
	}, nil
}

// ViaCallCount / LastRouteCall are race-safe accessors for assertions.
func (m *MockOSRM) ViaCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ViaCalls)
}

func (m *MockOSRM) RouteCallsSnapshot() [][4]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][4]float64, len(m.RouteCalls))
	copy(out, m.RouteCalls)
	return out
}

func (m *MockOSRM) straightLine(originLat, originLng, destLat, destLng float64) *services.RouteResult {
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
	}
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
