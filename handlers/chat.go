package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
)

func SendMessage(c *gin.Context) {
	var req struct {
		RideRequestID string `json:"ride_request_id" binding:"required"`
		Content       string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	senderID := c.GetString("user_id")

	// Verify sender is part of this ride request
	var driverID, passengerID string
	err := db.DB.QueryRow(context.Background(),
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`,
		req.RideRequestID,
	).Scan(&driverID, &passengerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return
	}
	if senderID != driverID && senderID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not part of this ride"})
		return
	}

	var msgID string
	var createdAt time.Time
	err = db.DB.QueryRow(context.Background(), `
		INSERT INTO messages (ride_request_id, sender_id, content)
		VALUES ($1, $2, $3)
		RETURNING id, created_at
	`, req.RideRequestID, senderID, req.Content).Scan(&msgID, &createdAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	msg := models.Message{
		ID:            msgID,
		RideRequestID: req.RideRequestID,
		SenderID:      senderID,
		Content:       req.Content,
		CreatedAt:     createdAt,
	}

	// Deliver to the other party via WebSocket
	recipientID := passengerID
	if senderID == passengerID {
		recipientID = driverID
	}
	Hub.SendToUser(recipientID, "chat_message", msg)

	// Mirror the WS push via FCM so a backgrounded recipient still gets the
	// notification. Best-effort: if the recipient has no token or FCM isn't
	// configured, NotifyChatMessage is a no-op.
	var recipientToken, senderName string
	_ = db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`,
		recipientID,
	).Scan(&recipientToken)
	_ = db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(name, '') FROM users WHERE id = $1`,
		senderID,
	).Scan(&senderName)
	go services.NotifyChatMessage(recipientToken, senderID, senderName, req.RideRequestID, req.Content)

	c.JSON(http.StatusOK, msg)
}

func GetMessages(c *gin.Context) {
	requestID := c.Param("request_id")
	userID := c.GetString("user_id")

	// Verify user is part of this ride request
	var driverID, passengerID string
	err := db.DB.QueryRow(context.Background(),
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`,
		requestID,
	).Scan(&driverID, &passengerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return
	}
	if userID != driverID && userID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not part of this ride"})
		return
	}

	rows, err := db.DB.Query(context.Background(), `
		SELECT id, ride_request_id, sender_id, content, created_at, read_at
		FROM messages
		WHERE ride_request_id = $1
		ORDER BY created_at ASC
	`, requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	var msgs []models.Message
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.RideRequestID, &m.SenderID, &m.Content, &m.CreatedAt, &m.ReadAt); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan error"})
			return
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []models.Message{}
	}
	c.JSON(http.StatusOK, msgs)
}

// TypingStatus broadcasts a transient "X is typing" hint to the other party
// over WebSocket. No persistence — the client debounces at the keystroke layer
// and stops sending after ~3 s of inactivity. The recipient's UI shows the
// pill for a fixed window after each "typing:true" event.
func TypingStatus(c *gin.Context) {
	var req struct {
		RideRequestID string `json:"ride_request_id" binding:"required"`
		Typing        bool   `json:"typing"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	senderID := c.GetString("user_id")

	var driverID, passengerID string
	err := db.DB.QueryRow(context.Background(),
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`,
		req.RideRequestID,
	).Scan(&driverID, &passengerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return
	}
	if senderID != driverID && senderID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not part of this ride"})
		return
	}

	recipientID := passengerID
	if senderID == passengerID {
		recipientID = driverID
	}
	Hub.SendToUser(recipientID, "chat_typing", gin.H{
		"ride_request_id": req.RideRequestID,
		"user_id":         senderID,
		"typing":          req.Typing,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// MarkRead stamps every message in the given ride that was sent by the OTHER
// party as read (now()) and notifies the sender so their ✓ → ✓✓ flips. Idempotent;
// re-calling on already-read messages is a no-op.
func MarkRead(c *gin.Context) {
	var req struct {
		RideRequestID string `json:"ride_request_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	readerID := c.GetString("user_id")

	var driverID, passengerID string
	err := db.DB.QueryRow(context.Background(),
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`,
		req.RideRequestID,
	).Scan(&driverID, &passengerID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "ride request not found"})
		return
	}
	if readerID != driverID && readerID != passengerID {
		c.JSON(http.StatusForbidden, gin.H{"error": "not part of this ride"})
		return
	}

	now := time.Now()
	rows, err := db.DB.Query(context.Background(), `
		UPDATE messages
		SET read_at = $1
		WHERE ride_request_id = $2
		  AND sender_id != $3
		  AND read_at IS NULL
		RETURNING id
	`, now, req.RideRequestID, readerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"updated": 0})
		return
	}

	// Notify the sender (the other party) so their UI flips the tick marks.
	senderID := passengerID
	if readerID == passengerID {
		senderID = driverID
	}
	Hub.SendToUser(senderID, "message_read", gin.H{
		"ride_request_id": req.RideRequestID,
		"message_ids":     ids,
		"read_at":         now,
	})
	c.JSON(http.StatusOK, gin.H{"updated": len(ids)})
}
