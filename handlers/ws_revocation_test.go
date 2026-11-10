package handlers_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pavelbotsu/sameway_backend/handlers"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitClosed reads until the server closes the socket (or the deadline).
func waitClosed(t *testing.T, conn *websocket.Conn, within time.Duration) error {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return err
		}
	}
}

func TestWS_SuspensionClosesOpenSocket(t *testing.T) {
	old := handlers.WSAccountRecheck
	handlers.WSAccountRecheck = 100 * time.Millisecond
	t.Cleanup(func() { handlers.WSAccountRecheck = old })

	pool, r := setupHandlers(t)
	id, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	conn := testutil.DialWS(t, testutil.ServeWS(t, r), tok)
	defer conn.Close()

	suspend(t, id)
	err := waitClosed(t, conn, 3*time.Second)
	var ce *websocket.CloseError
	require.True(t, errors.As(err, &ce), "want a close frame, got %v", err)
	assert.Equal(t, websocket.ClosePolicyViolation, ce.Code)
}

func TestWS_ActiveAccountStaysConnected(t *testing.T) {
	old := handlers.WSAccountRecheck
	handlers.WSAccountRecheck = 100 * time.Millisecond
	t.Cleanup(func() { handlers.WSAccountRecheck = old })

	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	conn := testutil.DialWS(t, testutil.ServeWS(t, r), tok)
	defer conn.Close()

	// Several rechecks pass; the read only ends by our own deadline.
	err := waitClosed(t, conn, 600*time.Millisecond)
	var ne interface{ Timeout() bool }
	require.True(t, errors.As(err, &ne) && ne.Timeout(), "socket closed unexpectedly: %v", err)
}

func TestWS_PasswordChangeClosesOldSockets(t *testing.T) {
	pool, r := setupHandlers(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "p1")
	conn := testutil.DialWS(t, testutil.ServeWS(t, r), tok)
	defer conn.Close()

	w := testutil.PostJSON(t, r, "/auth/change-password", tok, map[string]any{
		"current_password": "test-pass", "new_password": "new-pass-123",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Error(t, waitClosed(t, conn, 3*time.Second))
}
