package handlers_test

import (
	"net/http"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChat_SendAndFetch(t *testing.T) {
	pool, r := setupHandlers(t)
	driverID, driverTok := testutil.CreateUser(t, pool, "driver", "d1")
	passengerID, paxTok := testutil.CreateUser(t, pool, "passenger", "p1")
	reqID, _, err := services.CreateRideRequest(driverID, passengerID, 50, 20)
	require.NoError(t, err)

	w := testutil.PostJSON(t, r, "/chat/send", driverTok, map[string]any{
		"ride_request_id": reqID, "content": "hi",
	})
	require.Equal(t, http.StatusOK, w.Code)

	w2 := testutil.GetJSON(t, r, "/chat/"+reqID, paxTok)
	require.Equal(t, http.StatusOK, w2.Code)
	var msgs []map[string]any
	testutil.DecodeJSON(t, w2, &msgs)
	require.Len(t, msgs, 1)
	assert.Equal(t, "hi", msgs[0]["content"])
}
