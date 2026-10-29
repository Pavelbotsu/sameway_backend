package handlers_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── /safety/flag ───────────────────────────────────────────────────────────
//
// Raised when a rider answers "no, something is wrong" to the mid-ride
// check-in. The bar for this endpoint is different from the rest of the API:
// it must record what it can and otherwise get out of the way, because the
// client's very next action is opening the SOS sheet.

func TestFlagRideSafety_PassengerCanFlag(t *testing.T) {
	reqID, _, _, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", paxTok, map[string]any{
		"request_id": reqID, "reason": "check_in_not_ok",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var flaggedAt *time.Time
	var flaggedBy *string
	var reason *string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flagged_at, safety_flagged_by, safety_flag_reason
		 FROM ride_requests WHERE id = $1`, reqID,
	).Scan(&flaggedAt, &flaggedBy, &reason))
	require.NotNil(t, flaggedAt)
	require.NotNil(t, flaggedBy)
	require.NotNil(t, reason)
	assert.Equal(t, "check_in_not_ok", *reason)
}

func TestFlagRideSafety_DriverCanFlag(t *testing.T) {
	reqID, _, driverTok, _ := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", driverTok, map[string]any{
		"request_id": reqID, "reason": "check_in_not_ok",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var flaggedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flagged_at FROM ride_requests WHERE id = $1`, reqID,
	).Scan(&flaggedAt))
	assert.NotNil(t, flaggedAt)
}

// A ride id is guessable/replayable, so membership is verified against the row
// rather than trusted from the caller — otherwise anyone with an account could
// stamp arbitrary rides.
func TestFlagRideSafety_NonParticipantForbidden(t *testing.T) {
	reqID, _, _, _ := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()
	_, strangerTok := testutil.CreateUser(t, pool, "passenger", "safety-stranger")

	w := testutil.PostJSON(t, r, "/safety/flag", strangerTok, map[string]any{
		"request_id": reqID, "reason": "nosy",
	})
	require.Equal(t, http.StatusForbidden, w.Code)

	var flaggedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flagged_at FROM ride_requests WHERE id = $1`, reqID,
	).Scan(&flaggedAt))
	assert.Nil(t, flaggedAt, "a non-participant must not be able to stamp the ride")
}

// First flag wins: a double-tap or a client retry must not overwrite who
// raised it or when.
func TestFlagRideSafety_IsIdempotent(t *testing.T) {
	reqID, _, driverTok, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", paxTok, map[string]any{
		"request_id": reqID, "reason": "first",
	})
	require.Equal(t, http.StatusOK, w.Code)

	var firstAt time.Time
	var firstBy string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flagged_at, safety_flagged_by FROM ride_requests WHERE id = $1`,
		reqID).Scan(&firstAt, &firstBy))

	// Second flag, different party, different reason.
	w = testutil.PostJSON(t, r, "/safety/flag", driverTok, map[string]any{
		"request_id": reqID, "reason": "second",
	})
	// Still 200 — "already recorded" is a success from the caller's side, and a
	// safety control must not fail loudly at the moment someone is frightened.
	require.Equal(t, http.StatusOK, w.Code)

	var secondAt time.Time
	var secondBy string
	var reason string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flagged_at, safety_flagged_by, safety_flag_reason
		 FROM ride_requests WHERE id = $1`, reqID).Scan(&secondAt, &secondBy, &reason))
	assert.Equal(t, firstAt, secondAt, "timestamp must not be overwritten")
	assert.Equal(t, firstBy, secondBy, "flagger must not be overwritten")
	assert.Equal(t, "first", reason, "reason must not be overwritten")
}

func TestFlagRideSafety_UnknownRide(t *testing.T) {
	_, _, _, paxTok := seedAcceptedRide(t)
	r := testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", paxTok, map[string]any{
		"request_id": "00000000-0000-0000-0000-000000000000",
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFlagRideSafety_RequiresAuth(t *testing.T) {
	reqID, _, _, _ := seedAcceptedRide(t)
	r := testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", "", map[string]any{
		"request_id": reqID,
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// An empty reason stores NULL rather than an empty string, so a review query
// can distinguish "no reason given" from "reason was blank".
func TestFlagRideSafety_EmptyReasonStoresNull(t *testing.T) {
	reqID, _, _, paxTok := seedAcceptedRide(t)
	pool, r := handlersPool, testutil.NewTestRouter()

	w := testutil.PostJSON(t, r, "/safety/flag", paxTok, map[string]any{
		"request_id": reqID,
	})
	require.Equal(t, http.StatusOK, w.Code)

	var reason *string
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT safety_flag_reason FROM ride_requests WHERE id = $1`, reqID,
	).Scan(&reason))
	assert.Nil(t, reason)
}
