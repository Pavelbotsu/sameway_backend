package handlers_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gin-gonic/gin"
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

// Smoke test for hub stability under reconnect churn (role switch, two
// devices): connections come and go while SendToUser is hammered concurrently,
// and the hub must still deliver afterwards.
//
// Scope, honestly: this does NOT reproduce the send-on-closed-channel race it
// was written alongside. That window is a few instructions wide between
// releasing the read lock and the channel send, and this test still passed
// against a deliberately reverted fix. The fix itself is correct by
// construction — close and send now both happen under the hub mutex, so
// "registered" and "channel open" are one atomic fact — rather than by
// demonstration here. Treat this as a regression guard on churn behaviour, not
// as proof of the race.
//
// The race detector would be the right tool, but it needs cgo and this
// environment has no C compiler.
func TestWS_ConcurrentReconnectAndSendDoesNotPanic(t *testing.T) {
	pool, r := setupHandlers(t)
	userID, tok := testutil.CreateUser(t, pool, "passenger", "ws-race")
	wsBase := testutil.ServeWS(t, r)

	var wg sync.WaitGroup

	// Senders: hammer the hub while connections come and go.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				handlers.Hub.SendToUser(userID, "ping", gin.H{"n": j})
			}
		}()
	}

	// Reconnector: each dial displaces the previous client and closes its
	// channel — exactly the eviction path that raced.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 15; j++ {
			c, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws?token="+tok, nil)
			if err != nil {
				continue
			}
			time.Sleep(2 * time.Millisecond)
			_ = c.Close()
		}
	}()

	wg.Wait()

	// Hub still works afterwards: a fresh connection receives a fresh event.
	conn := testutil.DialWS(t, wsBase, tok)
	time.Sleep(50 * time.Millisecond)
	handlers.Hub.SendToUser(userID, "after_race", gin.H{"ok": true})
	evtType, _ := testutil.ReadEvent(t, conn, 2*time.Second)
	require.Equal(t, "after_race", evtType)
}
