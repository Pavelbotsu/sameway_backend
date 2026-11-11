package services_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonical OSRM-shaped response (geojson coords are [lng, lat]).
const osrmHappyBody = `{
  "routes": [{
    "distance": 1234.5,
    "geometry": { "coordinates": [[20.0, 50.0], [20.1, 50.0]] }
  }]
}`

func TestOSRMClient_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/route/v1/driving/")
		_, _ = w.Write([]byte(osrmHappyBody))
	}))
	defer srv.Close()

	c := &services.OSRMClient{BaseURL: srv.URL}
	r, err := c.GetRoadRoute(50.0, 20.0, 50.0, 20.1)
	require.NoError(t, err)
	assert.InDelta(t, 1.2345, r.DistanceKm, 0.001)
	assert.True(t, strings.HasPrefix(r.WKT, "LINESTRING("))
	assert.Len(t, r.Coords, 2)
}

func TestOSRMClient_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &services.OSRMClient{BaseURL: srv.URL}
	_, err := c.GetRoadRoute(50.0, 20.0, 50.0, 20.1)
	// 5xx returns empty body → decode error.
	require.Error(t, err)
}

func TestOSRMClient_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	c := &services.OSRMClient{BaseURL: srv.URL}
	_, err := c.GetRoadRoute(50.0, 20.0, 50.0, 20.1)
	require.Error(t, err)
}

func TestOSRMClient_NoRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"routes":[]}`))
	}))
	defer srv.Close()

	c := &services.OSRMClient{BaseURL: srv.URL}
	_, err := c.GetRoadRoute(50.0, 20.0, 50.0, 20.1)
	require.ErrorContains(t, err, "no route found")
}

func TestDefaultRoutingClient_Wrapper(t *testing.T) {
	// Verify the package-level GetRoadRoute delegates to DefaultRoutingClient.
	prev := services.DefaultRoutingClient
	t.Cleanup(func() { services.DefaultRoutingClient = prev })

	called := false
	services.DefaultRoutingClient = stubRouter{onCall: func() {
		called = true
	}}
	_, _ = services.GetRoadRoute(0, 0, 0, 0)
	assert.True(t, called, "GetRoadRoute must call DefaultRoutingClient")
}

type stubRouter struct{ onCall func() }

func (s stubRouter) GetRoadRoute(_, _, _, _ float64) (*services.RouteResult, error) {
	s.onCall()
	return &services.RouteResult{}, nil
}

func (s stubRouter) GetRoute(_ string, _, _, _, _ float64) (*services.RouteResult, error) {
	s.onCall()
	return &services.RouteResult{}, nil
}

func (s stubRouter) GetRouteVia(_ string, _ ...[2]float64) (*services.RouteResult, error) {
	s.onCall()
	return &services.RouteResult{}, nil
}

// Self-hosted OSRM runs one profile per server, so walking routes must go
// to their own URL.
func TestOSRMClient_FootUsesItsOwnServer(t *testing.T) {
	var carHits, footHits int
	car := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		carHits++
		_, _ = w.Write([]byte(osrmHappyBody))
	}))
	defer car.Close()
	foot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		footHits++
		assert.Contains(t, r.URL.Path, "/route/v1/foot/")
		_, _ = w.Write([]byte(osrmHappyBody))
	}))
	defer foot.Close()

	c := &services.OSRMClient{BaseURL: car.URL, FootBaseURL: foot.URL}
	_, err := c.GetRoute("foot", 50.0, 20.0, 50.0, 20.1)
	require.NoError(t, err)
	_, err = c.GetRoute("driving", 50.0, 20.0, 50.0, 20.1)
	require.NoError(t, err)
	assert.Equal(t, 1, footHits)
	assert.Equal(t, 1, carHits)

	// Without a foot server, walking falls back to the main URL.
	c = &services.OSRMClient{BaseURL: car.URL}
	_, err = c.GetRoute("foot", 50.0, 20.0, 50.0, 20.1)
	require.NoError(t, err)
	assert.Equal(t, 2, carHits)
}
