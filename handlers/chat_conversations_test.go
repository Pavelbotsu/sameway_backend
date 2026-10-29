package handlers_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Conversation gating (pre-match candidate rule) ──────────────────────────

// A passenger may NOT open a conversation with an arbitrary driver who is
// neither a past match nor currently in their nearby-drivers view.
func TestOpenConversation_StrangerForbidden(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.0}, nil)
	// Driver has NO active route — invisible to the passenger's map.

	w := testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// A driver visible in the passenger's nearby view IS a valid conversation
// target (the "saw them on the map, confirm before requesting" flow).
func TestOpenConversation_NearbyDriverAllowed(t *testing.T) {
	pool, r := setupHandlers(t)
	_, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	testutil.SwapRouter(t, &testutil.MockOSRM{})

	w := testutil.PostJSON(t, r, "/driver/route", driverTok, map[string]any{
		"origin_lat": 50.0, "origin_lng": 20.0,
		"destination_lat": 50.0, "destination_lng": 20.1,
		"corridor_km": 1.0, "seats": 2,
	})
	require.Equal(t, http.StatusOK, w.Code)
	testutil.CreateActivePassenger(t, pool, paxID,
		testutil.LatLng{Lat: 50.0, Lng: 20.02}, nil)

	var driverID string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT driver_id FROM active_drivers LIMIT 1`).Scan(&driverID))

	w = testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	assert.NotEmpty(t, resp["conversation_id"])

	// Idempotent: reopening yields the SAME conversation.
	w = testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp2 map[string]any
	testutil.DecodeJSON(t, w, &resp2)
	assert.Equal(t, resp["conversation_id"], resp2["conversation_id"])
}

// Any historical ride_request between the pair keeps the channel open even
// when nobody is actively looking (post-ride "thanks / forgot my umbrella").
func TestOpenConversation_RideHistoryAllowed(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'cancelled')
	`, driverID, paxID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	assert.Equal(t, http.StatusOK, w.Code)
}

// A block wins over everything — including ride history.
func TestOpenConversation_BlockedForbidden(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'done')
	`, driverID, paxID)
	require.NoError(t, err)

	// Driver blocks the passenger…
	w := testutil.PostJSON(t, r, "/chat/block", driverTok, map[string]any{
		"user_id": paxID,
	})
	require.Equal(t, http.StatusOK, w.Code)

	// …so the passenger can no longer open/message either direction.
	w = testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	assert.Equal(t, http.StatusForbidden, w.Code)

	// Unblock restores it.
	w = testutil.DeleteJSON(t, r, "/chat/block/"+paxID, driverTok)
	require.Equal(t, http.StatusOK, w.Code)
	w = testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── Pre-ride rate limit ──────────────────────────────────────────────────────

func TestSendMessage_PreRideRateLimited(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	// Pair matched once but never completed a ride → limit applies.
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'cancelled')
	`, driverID, paxID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	testutil.DecodeJSON(t, w, &resp)
	convID := resp["conversation_id"].(string)

	// Default cap is 5/min — the 6th message inside the window must 429.
	for i := 0; i < 5; i++ {
		w = testutil.PostJSON(t, r, "/chat/send", paxTok, map[string]any{
			"conversation_id": convID, "content": fmt.Sprintf("msg %d", i),
		})
		require.Equal(t, http.StatusOK, w.Code, "message %d within the cap", i)
	}
	w = testutil.PostJSON(t, r, "/chat/send", paxTok, map[string]any{
		"conversation_id": convID, "content": "one too many",
	})
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// ── Retention sweep ──────────────────────────────────────────────────────────

// Idle conversations WITHOUT a completed ride are purged (messages cascade);
// same-age conversations WITH one are kept; recent no-ride ones survive.
func TestReapIdleConversations_PolicyMatrix(t *testing.T) {
	pool, r := setupHandlers(t)
	dA, _ := testutil.CreateUser(t, pool, "driver", "dA")
	pA, _ := testutil.CreateUser(t, pool, "passenger", "pA")
	dB, _ := testutil.CreateUser(t, pool, "driver", "dB")
	pB, _ := testutil.CreateUser(t, pool, "passenger", "pB")
	dC, _ := testutil.CreateUser(t, pool, "driver", "dC")
	pC, _ := testutil.CreateUser(t, pool, "passenger", "pC")
	_ = r

	mkConv := func(x, y, rideStatus string, ageDays int) string {
		if rideStatus != "" {
			_, err := pool.Exec(ctx(t), `
				INSERT INTO ride_requests (driver_id, passenger_id, status)
				VALUES ($1, $2, $3)`, x, y, rideStatus)
			require.NoError(t, err)
		}
		var id string
		require.NoError(t, pool.QueryRow(ctx(t), `
			INSERT INTO conversations (user_a, user_b, created_at, last_message_at)
			VALUES (LEAST($1::uuid,$2::uuid), GREATEST($1::uuid,$2::uuid),
			        NOW() - make_interval(days => $3),
			        NOW() - make_interval(days => $3))
			RETURNING id`, x, y, ageDays).Scan(&id))
		return id
	}

	idle := mkConv(dA, pA, "cancelled", 120) // no completed ride, idle → purge
	kept := mkConv(dB, pB, "done", 120)      // completed ride → keep forever
	fresh := mkConv(dC, pC, "", 5)           // no ride, but recent → keep

	handlers.ReapIdleConversationsOnce()

	var count int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT COUNT(*) FROM conversations WHERE id = $1`, idle).Scan(&count))
	assert.Zero(t, count, "idle no-ride conversation must be purged")
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT COUNT(*) FROM conversations WHERE id = $1`, kept).Scan(&count))
	assert.Equal(t, 1, count, "ride-history conversation must survive")
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT COUNT(*) FROM conversations WHERE id = $1`, fresh).Scan(&count))
	assert.Equal(t, 1, count, "recent conversation must survive")
}

// ── Conversation list ────────────────────────────────────────────────────────

func TestListConversations_ShapeAndUnread(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	paxID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	_, err := pool.Exec(ctx(t), `
		INSERT INTO ride_requests (driver_id, passenger_id, status)
		VALUES ($1, $2, 'done')`, driverID, paxID)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/chat/conversations", paxTok, map[string]any{
		"peer_id": driverID,
	})
	require.Equal(t, http.StatusOK, w.Code)
	var open map[string]any
	testutil.DecodeJSON(t, w, &open)

	w = testutil.PostJSON(t, r, "/chat/send", paxTok, map[string]any{
		"conversation_id": open["conversation_id"], "content": "hello there",
	})
	require.Equal(t, http.StatusOK, w.Code)

	// Driver's list: one thread, passenger as peer, 1 unread, ride history.
	w = testutil.GetJSON(t, r, "/chat/conversations", driverTok)
	require.Equal(t, http.StatusOK, w.Code)
	var list []map[string]any
	testutil.DecodeJSON(t, w, &list)
	require.Len(t, list, 1)
	assert.Equal(t, paxID, list[0]["peer_id"])
	assert.Equal(t, "hello there", list[0]["last_message"])
	assert.EqualValues(t, 1, list[0]["unread_count"])
	assert.Equal(t, true, list[0]["has_ride_history"])
}
