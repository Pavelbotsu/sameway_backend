package handlers_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRideShare_LifeCycle(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "Jan Kowalski")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "share-p")
	_, strangerTok := testutil.CreateUser(t, pool, "passenger", "stranger")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50.06, Lng: 19.94}, testutil.LatLng{Lat: 50.07, Lng: 20.0},
		testutil.LineStringWKT([2]float64{19.94, 50.06}, [2]float64{20.0, 50.07}))
	testutil.SetDriverCurrentLocation(t, pool, driverID, testutil.LatLng{Lat: 50.061, Lng: 19.95})
	rideID, _, err := services.CreateRideRequest(driverID, paxID, 50.065, 19.96)
	require.NoError(t, err)

	// Not before the ride is agreed.
	w := testutil.PostJSON(t, r, "/rides/"+rideID+"/share", paxTok, nil)
	assert.Equal(t, http.StatusConflict, w.Code)

	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'accepted' WHERE id = $1`, rideID)
	require.NoError(t, err)

	// Only the people in the ride can share it.
	assert.Equal(t, http.StatusNotFound, testutil.PostJSON(t, r, "/rides/"+rideID+"/share", strangerTok, nil).Code)

	w = testutil.PostJSON(t, r, "/rides/"+rideID+"/share", paxTok, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]string
	testutil.DecodeJSON(t, w, &resp)
	url := resp["url"]
	require.True(t, strings.HasPrefix(url, "/s/"), url)

	// The public page and its live data, no sign-in needed.
	page := testutil.GetJSON(t, r, url, "")
	assert.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "leaflet")

	live := testutil.GetJSON(t, r, url+"/live", "")
	require.Equal(t, http.StatusOK, live.Code, live.Body.String())
	var d map[string]any
	testutil.DecodeJSON(t, live, &d)
	assert.Equal(t, "accepted", d["status"])
	assert.Equal(t, "Jan", d["driver_name"], "first name only")
	assert.Equal(t, "KR 00000", d["car_plate"])
	assert.NotNil(t, d["car"], "live car position")
	assert.NotNil(t, d["pickup"])

	// Once the ride is over, the link stops showing anything.
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'done' WHERE id = $1`, rideID)
	require.NoError(t, err)
	assert.Equal(t, http.StatusGone, testutil.GetJSON(t, r, url+"/live", "").Code)

	// Expired links too.
	_, err = pool.Exec(ctx(t), `UPDATE ride_requests SET status = 'in_progress' WHERE id = $1`, rideID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(t), `UPDATE ride_shares SET expires_at = NOW() - INTERVAL '1 minute'`)
	require.NoError(t, err)
	assert.Equal(t, http.StatusGone, testutil.GetJSON(t, r, url+"/live", "").Code)
}

func TestRideShare_UnknownTokens(t *testing.T) {
	_, r := setupHandlers(t)
	assert.Equal(t, http.StatusNotFound, testutil.GetJSON(t, r, "/s/not-a-token", "").Code)
	assert.Equal(t, http.StatusNotFound,
		testutil.GetJSON(t, r, "/s/0123456789abcdef0123456789abcdef/live", "").Code)
}
