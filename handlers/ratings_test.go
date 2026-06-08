package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubmitRating_Persists(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/ratings", tok, map[string]any{
		"rated_id":        driverID,
		"ride_request_id": reqID,
		"stars":           5,
		"comment":         "great",
	})
	require.Equal(t, http.StatusCreated, w.Code)

	var stars int
	require.NoError(t, pool.QueryRow(ctx(t),
		`SELECT stars FROM ratings WHERE ride_request_id = $1`, reqID).Scan(&stars))
	assert.Equal(t, 5, stars)
}

func TestSubmitRating_RejectsSelfRating(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, _ := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/ratings", driverTok, map[string]any{
		"rated_id":        driverID, // self
		"ride_request_id": reqID,
		"stars":           5,
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSubmitRating_BadStars(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, _ := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/ratings", tok, map[string]any{
		"rated_id":        driverID,
		"ride_request_id": reqID,
		"stars":           7, // out of range
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
