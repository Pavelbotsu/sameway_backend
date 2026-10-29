package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
)

type RouteResult struct {
	WKT        string
	Coords     [][2]float64
	DistanceKm float64
	// Durations holds OSRM's per-segment travel time in seconds, aligned with
	// Coords: Durations[i] is the time from Coords[i] to Coords[i+1], so
	// len(Durations) == len(Coords)-1. Empty when the OSRM deployment doesn't
	// return duration annotations. Used to build the measured route line for
	// ETA-to-pickup interpolation (see CumulativeDurations / matching).
	Durations []float64
	// DurationS is the total route travel time in seconds (sum of Durations,
	// or OSRM's route-level duration when annotations are absent).
	DurationS float64
}

// CumulativeDurations turns per-segment seconds into cumulative seconds per
// vertex: returns a slice of len(segments)+1 where out[0]=0 and out[i] is the
// total time from the start to vertex i. Used to build the LINESTRING M whose
// M dimension carries travel time.
func CumulativeDurations(segments []float64) []float64 {
	out := make([]float64, len(segments)+1)
	for i, s := range segments {
		out[i+1] = out[i] + s
	}
	return out
}

// RoutingClient abstracts road-route lookups so tests can swap a mock without
// hitting OSRM. The production wiring (DefaultRoutingClient) is set at startup
// to an OSRMClient that reads config.C.OSRMBaseURL on each call.
//
// GetRoute takes an OSRM profile name ("driving", "foot", "bike"). Many OSRM
// deployments only have the driving profile compiled in — implementations
// should fall back to a straight-line WKT for unsupported profiles rather
// than erroring, so the caller's mini-map can still render something useful.
type RoutingClient interface {
	GetRoadRoute(originLat, originLng, destLat, destLng float64) (*RouteResult, error)
	GetRoute(profile string, originLat, originLng, destLat, destLng float64) (*RouteResult, error)
	// GetRouteVia routes through every point in order (points are (lat, lng)
	// pairs; at least two). Used by the micro-detour evaluation: origin →
	// passenger → destination in one OSRM call.
	GetRouteVia(profile string, points ...[2]float64) (*RouteResult, error)
}

// OSRMClient is the production implementation. BaseURL falls back to
// config.C.OSRMBaseURL when empty so existing code paths keep working.
type OSRMClient struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *OSRMClient) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return config.C.OSRMBaseURL
}

// osrmDefaultClient bounds every OSRM call. Routing is on the hot path of
// SetRoute/PassengerSearch/GetPickupRoute — an unbounded http.DefaultClient
// call would pin handler goroutines for as long as a hung upstream likes.
var osrmDefaultClient = &http.Client{Timeout: 8 * time.Second}

// osrmSem caps how many OSRM requests are in flight across the whole process.
//
// Per-call timeouts bound each request but say nothing about how MANY run at
// once, and the callers fan out: every driver location update can spawn a
// corridor re-run, every passenger ping a reverse-match, and each of those may
// evaluate detour routes. With N active users that is unbounded concurrency
// against a single upstream — which is how you get rate-limited (or, on a
// self-hosted instance, how you saturate it). Requests queue here rather than
// stampeding; the 8s client timeout still bounds total wait.
var osrmSem = make(chan struct{}, 4)

func acquireOSRM() { osrmSem <- struct{}{} }
func releaseOSRM() { <-osrmSem }

func (c *OSRMClient) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return osrmDefaultClient
}

func (c *OSRMClient) GetRoadRoute(originLat, originLng, destLat, destLng float64) (*RouteResult, error) {
	return c.GetRoute("driving", originLat, originLng, destLat, destLng)
}

// GetRoute fetches a route via the given OSRM profile. Profile names match
// OSRM's URL segment: "driving", "foot", "bike". If the deployed OSRM doesn't
// have the requested profile compiled in, the call surfaces an error and the
// caller is expected to gracefully degrade (the pickup-route handler returns
// an empty WKT for that leg rather than failing the whole response).
func (c *OSRMClient) GetRoute(profile string, originLat, originLng, destLat, destLng float64) (*RouteResult, error) {
	return c.GetRouteVia(profile, [2]float64{originLat, originLng}, [2]float64{destLat, destLng})
}

// GetRouteVia is the multi-waypoint variant (OSRM takes any number of
// lng,lat pairs separated by ';').
func (c *OSRMClient) GetRouteVia(profile string, points ...[2]float64) (*RouteResult, error) {
	if profile == "" {
		profile = "driving"
	}
	if len(points) < 2 {
		return nil, fmt.Errorf("osrm: need at least 2 waypoints, got %d", len(points))
	}
	waypoints := make([]string, len(points))
	for i, p := range points {
		waypoints[i] = fmt.Sprintf("%f,%f", p[1], p[0]) // OSRM wants lng,lat
	}
	// annotations=duration returns per-segment travel times aligned with the
	// full geometry, which we interpolate into ETA-to-pickup without per-
	// candidate OSRM calls. Harmless on deployments that ignore it (the field
	// just comes back empty and callers fall back to route-level duration).
	url := fmt.Sprintf(
		"%s/route/v1/%s/%s?overview=full&geometries=geojson&annotations=duration",
		c.baseURL(), profile, strings.Join(waypoints, ";"),
	)

	acquireOSRM()
	resp, err := c.http().Get(url)
	releaseOSRM()
	if err != nil {
		return nil, fmt.Errorf("osrm request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The public demo server rate-limits with non-200s; surfacing the
		// status beats decoding garbage into "no route found".
		return nil, fmt.Errorf("osrm http %d", resp.StatusCode)
	}

	var osrm struct {
		Routes []struct {
			Distance float64 `json:"distance"`
			Duration float64 `json:"duration"`
			Geometry struct {
				Coordinates [][2]float64 `json:"coordinates"`
			} `json:"geometry"`
			Legs []struct {
				Annotation struct {
					Duration []float64 `json:"duration"`
				} `json:"annotation"`
			} `json:"legs"`
		} `json:"routes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&osrm); err != nil {
		return nil, fmt.Errorf("osrm decode: %w", err)
	}
	if len(osrm.Routes) == 0 {
		return nil, fmt.Errorf("no route found")
	}

	route := osrm.Routes[0]
	coords := route.Geometry.Coordinates
	parts := make([]string, len(coords))
	for i, c := range coords {
		parts[i] = fmt.Sprintf("%f %f", c[0], c[1])
	}
	wkt := "LINESTRING(" + strings.Join(parts, ", ") + ")"

	// Flatten per-leg duration annotations into one segment array aligned with
	// the geometry (we only ever request 2 waypoints → 1 leg, but concatenate
	// defensively).
	var durations []float64
	for _, leg := range route.Legs {
		durations = append(durations, leg.Annotation.Duration...)
	}

	return &RouteResult{
		WKT:        wkt,
		Coords:     coords,
		DistanceKm: route.Distance / 1000,
		Durations:  durations,
		DurationS:  route.Duration,
	}, nil
}

// DefaultRoutingClient is the package-level routing client every caller goes
// through. Tests assign a mock implementation here in TestMain or per-test.
var DefaultRoutingClient RoutingClient = &OSRMClient{}

// GetRoadRoute is a thin wrapper that delegates to DefaultRoutingClient so
// existing handler code can keep calling it unchanged.
func GetRoadRoute(originLat, originLng, destLat, destLng float64) (*RouteResult, error) {
	return DefaultRoutingClient.GetRoadRoute(originLat, originLng, destLat, destLng)
}

// GetRoute is the profile-aware variant. New callers should prefer this over
// GetRoadRoute; the latter remains for backward compat.
func GetRoute(profile string, originLat, originLng, destLat, destLng float64) (*RouteResult, error) {
	return DefaultRoutingClient.GetRoute(profile, originLat, originLng, destLat, destLng)
}

// GetRouteVia routes through every (lat, lng) point in order.
func GetRouteVia(profile string, points ...[2]float64) (*RouteResult, error) {
	return DefaultRoutingClient.GetRouteVia(profile, points...)
}
