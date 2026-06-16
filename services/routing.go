package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Pavelbotsu/sameway_backend/config"
)

type RouteResult struct {
	WKT        string
	Coords     [][2]float64
	DistanceKm float64
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

func (c *OSRMClient) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
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
	if profile == "" {
		profile = "driving"
	}
	url := fmt.Sprintf(
		"%s/route/v1/%s/%f,%f;%f,%f?overview=full&geometries=geojson",
		c.baseURL(), profile, originLng, originLat, destLng, destLat,
	)

	resp, err := c.http().Get(url)
	if err != nil {
		return nil, fmt.Errorf("osrm request: %w", err)
	}
	defer resp.Body.Close()

	var osrm struct {
		Routes []struct {
			Distance float64 `json:"distance"`
			Geometry struct {
				Coordinates [][2]float64 `json:"coordinates"`
			} `json:"geometry"`
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

	return &RouteResult{
		WKT:        wkt,
		Coords:     coords,
		DistanceKm: route.Distance / 1000,
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
