package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// events reads a socket in the background (a read timeout would break a
// gorilla connection for good, so tests must not use read deadlines to
// check that nothing arrived).
func events(conn *websocket.Conn) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		for {
			var msg struct {
				Type string `json:"type"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			ch <- msg.Type
		}
	}()
	return ch
}

// gotEvent reports whether an event of type want arrives within d.
func gotEvent(ch <-chan string, want string, d time.Duration) bool {
	timeout := time.After(d)
	for {
		select {
		case typ, ok := <-ch:
			if !ok {
				return false
			}
			if typ == want {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

func TestNearbyHints_NearPassengersOnly_Throttled(t *testing.T) {
	pool, r := setupHandlers(t)
	testutil.SwapRouter(t, &testutil.MockOSRM{})
	wsBase := testutil.ServeWS(t, r)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "hint-d")
	nearID, nearTok := testutil.CreateUser(t, pool, "passenger", "hint-near")
	farID, farTok := testutil.CreateUser(t, pool, "passenger", "hint-far")
	testutil.CreateActivePassenger(t, pool, nearID, testutil.LatLng{Lat: 50.01, Lng: 20.01}, nil)
	testutil.CreateActivePassenger(t, pool, farID, testutil.LatLng{Lat: 49.59, Lng: 34.55}, nil) // Poltava
	near := events(testutil.DialWS(t, wsBase, nearTok))
	far := events(testutil.DialWS(t, wsBase, farTok))
	_ = driverID

	// Publishing a route hints passengers within 10 km.
	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0, "destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, gotEvent(near, "nearby_changed", 3*time.Second), "near passenger is told")
	assert.False(t, gotEvent(far, "nearby_changed", 500*time.Millisecond), "far passenger is not")

	// A big move right after: throttled (at most one hint per 30 s).
	w = testutil.PutJSON(t, r, "/driver/location", driverTok, map[string]any{"lat": 50.0, "lng": 20.05})
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, gotEvent(near, "nearby_changed", 700*time.Millisecond))

	// Ending the route hints again (passengers drop the driver from the map).
	w = testutil.DeleteJSON(t, r, "/driver/route", driverTok)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, gotEvent(near, "nearby_changed", 3*time.Second))
}
