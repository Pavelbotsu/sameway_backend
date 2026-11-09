package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Blocks apply to matching, not only to chat (audit P1-4) ─────────────────

func block(t *testing.T, blocker, blocked string) {
	t.Helper()
	_, err := handlersPool.Exec(ctx(t),
		`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)`, blocker, blocked)
	require.NoError(t, err)
}

func TestBlockedDriver_HiddenFromNearby(t *testing.T) {
	for _, dir := range []string{"passenger blocks driver", "driver blocks passenger"} {
		t.Run(dir, func(t *testing.T) {
			pool, r := setupHandlers(t)
			driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
			testutil.CreateActiveDriver(t, pool, driverID,
				testutil.LatLng{Lat: 50.0, Lng: 20.0}, testutil.LatLng{Lat: 50.0, Lng: 20.1},
				testutil.LineStringWKT([2]float64{20.0, 50.0}, [2]float64{20.1, 50.0}))
			paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
			testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)

			if dir == "passenger blocks driver" {
				block(t, paxID, driverID)
			} else {
				block(t, driverID, paxID)
			}
			w := testutil.PostJSON(t, r, "/passenger/nearby-drivers", paxTok, map[string]any{
				"latitude": 50.0, "longitude": 20.02, "radius_km": 10.0,
			})
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var list []map[string]any
			testutil.DecodeJSON(t, w, &list)
			assert.Empty(t, list)
		})
	}
}

func TestBlockedPassenger_GetsNoCorridorOffer(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})
	dest := testutil.LatLng{Lat: 50.0, Lng: 20.09}
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.02}, &dest)
	block(t, paxID, driverID)

	wsBase := testutil.ServeWS(t, r)
	paxConn := testutil.DialWS(t, wsBase, paxTok)
	time.Sleep(100 * time.Millisecond)

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, driverRouteBody())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.EqualValues(t, 0, resp["notified_count"])
	testutil.ExpectNoEvent(t, paxConn, 500*time.Millisecond)
}

func TestBlockedPair_CannotRequestDriver(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	testutil.CreateActiveDriver(t, pool, driverID,
		testutil.LatLng{Lat: 50.0, Lng: 20.0}, testutil.LatLng{Lat: 50.0, Lng: 20.1},
		testutil.LineStringWKT([2]float64{20.0, 50.0}, [2]float64{20.1, 50.0}))
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, paxID, testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)
	block(t, driverID, paxID)

	w := testutil.PostJSON(t, r, "/passenger/request-driver", paxTok, map[string]any{"driver_id": driverID})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "driver is not available")
}
