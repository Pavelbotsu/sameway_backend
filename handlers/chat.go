package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
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
		SELECT id, ride_request_id, sender_id, content, created_at
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
		if err := rows.Scan(&m.ID, &m.RideRequestID, &m.SenderID, &m.Content, &m.CreatedAt); err != nil {
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
