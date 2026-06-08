package testutil

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// ServeWS wraps a Gin engine in an httptest.NewServer and returns a "ws://…"
// base URL. The server is shut down on cleanup.
func ServeWS(t testing.TB, r *gin.Engine) string {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// DialWS opens a WebSocket connection to /ws with a token query param.
// Returns the connection — caller is responsible for Close.
func DialWS(t testing.TB, wsBase, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsBase+"/ws?token="+token, nil)
	require.NoError(t, err, "dial ws")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ReadEvent blocks up to `timeout` for the next JSON frame on conn and decodes
// it. Returns (type, payload). Fails the test on timeout.
func ReadEvent(t testing.TB, conn *websocket.Conn, timeout time.Duration) (string, map[string]any) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err, "read ws message within %s", timeout)
	var msg struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(raw, &msg), "decode ws message: %s", string(raw))
	return msg.Type, msg.Payload
}

// ExpectNoEvent asserts that no frame arrives within `wait`. Useful for
// idempotency tests: "a second RequestDriver should NOT emit another
// passenger_request to the driver".
func ExpectNoEvent(t testing.TB, conn *websocket.Conn, wait time.Duration) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("expected no ws event, got one")
	}
	// Any error here is acceptable — it's a timeout/closed/etc. The point is
	// the read failed to deliver a frame.
}
