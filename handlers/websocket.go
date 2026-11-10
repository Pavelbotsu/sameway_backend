package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

// Standard gorilla keepalive parameters: the server pings every pingPeriod;
// a connection that hasn't ponged within pongWait is considered dead and torn
// down, so mobile clients that vanish without FIN free their hub slot in
// ~1 minute instead of the OS TCP timeout.
const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 54 * time.Second // must be < wsPongWait
)

// WSAccountRecheck is how often an open socket re-reads its account. Logout,
// password change and deletion close sockets directly; this catches the rest
// (suspension set by SQL, token_version bumped elsewhere) within a minute.
// Variable so tests can shorten it.
var WSAccountRecheck = time.Minute

var upgrader = websocket.Upgrader{
	// Mobile app clients send no Origin header. Browser clients do — reject
	// unknown origins to prevent cross-site WebSocket hijacking.
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == "" || r.Host == hostOf(r.Header.Get("Origin"))
	},
}

func hostOf(origin string) string {
	origin = strings.TrimPrefix(origin, "https://")
	origin = strings.TrimPrefix(origin, "http://")
	if i := strings.IndexByte(origin, '/'); i >= 0 {
		origin = origin[:i]
	}
	return origin
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
	// closeOnce guards `send` against a double close. Two paths can retire the
	// same client — register() displacing it, and its own deferred
	// unregister() — and closing an already-closed channel panics.
	closeOnce sync.Once
}

// closeSend retires the client's outbound channel exactly once.
func (c *wsClient) closeSend() {
	c.closeOnce.Do(func() { close(c.send) })
}

type WSHub struct {
	mu      sync.RWMutex
	clients map[string]*wsClient
}

var Hub = &WSHub{
	clients: make(map[string]*wsClient),
}

// register installs the client as the user's single live connection. A
// displaced previous connection (reconnect, second device) is closed so its
// read loop exits; its own deferred unregister is then a no-op because the
// map no longer points at it.
func (h *WSHub) register(userID string, client *wsClient) {
	h.mu.Lock()
	old := h.clients[userID]
	h.clients[userID] = client
	// The close happens under the SAME lock SendToUser holds while sending, so
	// a concurrent send can never observe a half-retired client. Previously
	// this ran after Unlock, leaving a window in which SendToUser had already
	// read the client pointer and then sent on a channel we had just closed —
	// a "send on closed channel" panic that surfaced as a 500 on an operation
	// that had already committed.
	if old != nil {
		old.closeSend()
	}
	h.mu.Unlock()
	if old != nil {
		// Closing the conn can block; keep it outside the lock.
		old.conn.Close()
	}
}

// unregister removes the client ONLY if it is still the registered one —
// a stale connection's teardown must never evict the live session that
// replaced it (that was a real bug: reconnects silently lost all WS events).
func (h *WSHub) unregister(userID string, client *wsClient) {
	h.mu.Lock()
	if h.clients[userID] == client {
		delete(h.clients, userID)
		client.closeSend()
	}
	h.mu.Unlock()
}

// CloseAll retires every live connection. Called during graceful shutdown:
// closing each client's send channel makes its writer goroutine emit a
// CloseGoingAway frame, so clients reconnect on their normal backoff instead of
// seeing the socket disappear. Without this, http.Server.Shutdown would simply
// block on the long-lived WS connections until the drain deadline expired.
func (h *WSHub) CloseAll() {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for userID, client := range h.clients {
		client.closeSend()
		conns = append(conns, client.conn)
		delete(h.clients, userID)
	}
	h.mu.Unlock()
	// Give the writers a moment to flush their close frames before the
	// underlying conns go away.
	time.Sleep(100 * time.Millisecond)
	for _, c := range conns {
		c.Close()
	}
}

// Disconnect retires the user's live connection, if any. Used when the account
// is deleted: the token_version bump only stops NEW connections, so without
// this the deleted user's open socket would keep receiving events.
func (h *WSHub) Disconnect(userID string) {
	h.mu.Lock()
	client := h.clients[userID]
	if client != nil {
		delete(h.clients, userID)
		client.closeSend()
	}
	h.mu.Unlock()
	if client != nil {
		client.conn.Close()
	}
}

func (h *WSHub) SendToUser(userID, msgType string, payload interface{}) {
	// Marshal BEFORE taking the lock — no reason to hold it across encoding.
	data, err := json.Marshal(models.WSMessage{Type: msgType, Payload: payload})
	if err != nil {
		return
	}
	// The send stays under RLock: register/unregister close `send` while
	// holding the write lock, so holding the read lock here makes "still
	// registered" and "channel still open" one atomic fact. The send itself is
	// non-blocking (default branch), so this never holds the lock on a slow
	// consumer.
	h.mu.RLock()
	defer h.mu.RUnlock()
	client, ok := h.clients[userID]
	if !ok {
		return
	}
	select {
	case client.send <- data:
	default:
		// Dropping is the backpressure policy (FCM + REST refetch are the
		// recovery paths), but never drop silently — these are state events.
		log.Printf("ws: dropping %q to user %s (send buffer full)", msgType, userID)
	}
}

func HandleWS(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	userID, _, tv, err := middleware.ParseToken(token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}
	// Same revocation check the HTTP middleware runs. Without it a token
	// invalidated by a password change or role switch could still open a live
	// event stream for the account.
	st, ok := middleware.LoadAccountStatus(c.Request.Context(), userID)
	if !ok || st.TokenVersion != tv {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
		return
	}
	if st.Suspended {
		c.JSON(http.StatusForbidden, gin.H{"error": "this account is suspended", "code": "account_suspended"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}

	client := &wsClient{conn: conn, send: make(chan []byte, 64)}
	// Greeting frame, queued before the client is reachable by anyone else so
	// it is always first. WS pings are control frames that most client
	// libraries never surface, so without a data frame the app could not tell
	// a working-but-quiet socket from one that never upgraded — and it counted
	// every ordinary drop as an auth failure and logged the user out.
	if hello, err := json.Marshal(models.WSMessage{Type: "hello", Payload: gin.H{}}); err == nil {
		client.send <- hello
	}
	Hub.register(userID, client)
	defer func() {
		Hub.unregister(userID, client)
		conn.Close()
	}()

	// Writer: owns all writes on this conn (messages + pings).
	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer ticker.Stop()
		recheck := time.NewTicker(WSAccountRecheck)
		defer recheck.Stop()
		for {
			select {
			case msg, ok := <-client.send:
				if !ok {
					conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseGoingAway, "replaced"),
						time.Now().Add(wsWriteWait))
					return
				}
				conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					return
				}
			case <-ticker.C:
				conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-recheck.C:
				st, err := middleware.ReadAccountStatus(context.Background(), userID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					continue // DB trouble: don't drop every socket, try next time
				}
				if err == nil && st.TokenVersion == tv && !st.Suspended {
					continue
				}
				// Session revoked: say so, then close so the reader loop ends
				// and the deferred unregister runs.
				conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session revoked"),
					time.Now().Add(wsWriteWait))
				conn.Close()
				return
			}
		}
	}()

	// Reader: we ignore client payloads; the loop exists to consume control
	// frames and detect disconnects via the pong-refreshed read deadline.
	conn.SetReadLimit(4096)
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}
