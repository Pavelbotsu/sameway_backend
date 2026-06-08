package handlers_test

import (
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWS_DialRejectsMissingToken(t *testing.T) {
	_, r := setupHandlers(t)
	wsBase := testutil.ServeWS(t, r)

	// Dial with no token at all — handshake must fail.
	_, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws", nil)
	require.Error(t, err, "ws handshake must fail when token is missing")
}

func TestWS_DialRejectsBadToken(t *testing.T) {
	_, r := setupHandlers(t)
	wsBase := testutil.ServeWS(t, r)

	_, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws?token=not-a-jwt", nil)
	require.Error(t, err)
}

func TestWS_HubSendToUser_DeliversToDialedConn(t *testing.T) {
	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	wsBase := testutil.ServeWS(t, r)

	conn := testutil.DialWS(t, wsBase, tok)

	// Give the registration time to land in the Hub.
	time.Sleep(100 * time.Millisecond)
	handlers.Hub.SendToUser(id, "test_event", map[string]any{"hello": "world"})

	evtType, payload := testutil.ReadEvent(t, conn, 2*time.Second)
	assert.Equal(t, "test_event", evtType)
	assert.Equal(t, "world", payload["hello"])
}
