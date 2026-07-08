package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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
	h.mu.Unlock()
	if old != nil {
		close(old.send)
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
		close(client.send)
	}
	h.mu.Unlock()
}

func (h *WSHub) SendToUser(userID, msgType string, payload interface{}) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()
	if !ok {
		return
	}
	data, err := json.Marshal(models.WSMessage{Type: msgType, Payload: payload})
	if err != nil {
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
	userID, _, err := middleware.ParseToken(token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}

	client := &wsClient{conn: conn, send: make(chan []byte, 64)}
	Hub.register(userID, client)
	defer func() {
		Hub.unregister(userID, client)
		conn.Close()
	}()

	// Writer: owns all writes on this conn (messages + pings).
	go func() {
		ticker := time.NewTicker(wsPingPeriod)
		defer ticker.Stop()
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
