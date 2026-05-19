package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
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

func (h *WSHub) register(userID string, client *wsClient) {
	h.mu.Lock()
	h.clients[userID] = client
	h.mu.Unlock()
}

func (h *WSHub) unregister(userID string) {
	h.mu.Lock()
	if c, ok := h.clients[userID]; ok {
		close(c.send)
		delete(h.clients, userID)
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
	}
}

func HandleWS(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
		return
	}
	claims, err := middleware.ParseToken(token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return
	}
	userID := claims["user_id"].(string)

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}

	client := &wsClient{conn: conn, send: make(chan []byte, 64)}
	Hub.register(userID, client)
	defer func() {
		Hub.unregister(userID)
		conn.Close()
	}()

	go func() {
		for msg := range client.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
}
