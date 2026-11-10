package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withLaunchAreas turns on the production service areas for one test.
func withLaunchAreas(t *testing.T) {
	t.Helper()
	areas, err := config.ParseAreas(config.DefaultServiceAreas)
	require.NoError(t, err)
	prev := config.C.ServiceAreas
	config.C.ServiceAreas = areas
	t.Cleanup(func() { config.C.ServiceAreas = prev })
}

func route(oLat, oLng, dLat, dLng float64) map[string]any {
	return map[string]any{
		"origin_lat": oLat, "origin_lng": oLng,
		"destination_lat": dLat, "destination_lng": dLng,
		"corridor_km": 1.0, "seats": 2,
	}
}

func TestServiceArea_Routes(t *testing.T) {
	pool, r := setupHandlers(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})
	withLaunchAreas(t)
	_, tok := testutil.CreateUser(t, pool, "driver", "area-d")

	// Inside Kraków and inside Poltava oblast: fine.
	w := testutil.PostJSON(t, r, "/driver/route", tok, route(50.0614, 19.9383, 50.0700, 19.9950))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = testutil.PostJSON(t, r, "/driver/route", tok, route(49.5883, 34.5514, 49.0659, 33.4200))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Warsaw → Kraków: origin outside.
	w = testutil.PostJSON(t, r, "/driver/route", tok, route(52.2297, 21.0122, 50.0614, 19.9383))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "outside_service_area")

	// Kraków → Poltava: both inside, but not the same area.
	w = testutil.PostJSON(t, r, "/driver/route", tok, route(50.0614, 19.9383, 49.5883, 34.5514))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestServiceArea_PassengerLocationAndNearby(t *testing.T) {
	pool, r := setupHandlers(t)
	withLaunchAreas(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "area-p")

	w := testutil.PutJSON(t, r, "/passenger/location", tok, map[string]any{"lat": 52.2297, "lng": 21.0122})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "outside_service_area")

	w = testutil.PutJSON(t, r, "/passenger/location", tok, map[string]any{"lat": 49.5883, "lng": 34.5514})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = testutil.PostJSON(t, r, "/passenger/nearby-drivers", tok, map[string]any{
		"latitude": 52.2297, "longitude": 21.0122, "radius_km": 10})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// No areas configured (dev, tests): everything allowed.
func TestServiceArea_NoLimitWhenUnset(t *testing.T) {
	pool, r := setupHandlers(t)
	prev := config.C.ServiceAreas
	config.C.ServiceAreas = nil
	t.Cleanup(func() { config.C.ServiceAreas = prev })
	_, tok := testutil.CreateUser(t, pool, "passenger", "area-none")
	w := testutil.PutJSON(t, r, "/passenger/location", tok, map[string]any{"lat": 52.2297, "lng": 21.0122})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
