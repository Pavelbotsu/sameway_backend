package handlers

import (
	"context"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
)

func SubmitRating(c *gin.Context) {
	var req struct {
		RatedID       string `json:"rated_id" binding:"required"`
		RideRequestID string `json:"ride_request_id" binding:"required"`
		Stars         int    `json:"stars" binding:"required,min=1,max=5"`
		Comment       string `json:"comment"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	raterID := c.GetString("user_id")
	if raterID == req.RatedID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot rate yourself"})
		return
	}

	_, err := db.DB.Exec(context.Background(), `
		INSERT INTO ratings (rater_id, rated_id, ride_request_id, stars, comment)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (rater_id, ride_request_id) DO NOTHING
	`, raterID, req.RatedID, req.RideRequestID, req.Stars, req.Comment)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}

	// Best-effort WS so the rated user knows the other party submitted; lets the
	// "Waiting for them to rate you" banner dismiss without the full 30 s timeout.
	var driverID, passengerID string
	if lookupErr := db.DB.QueryRow(c.Request.Context(),
		`SELECT driver_id, passenger_id FROM ride_requests WHERE id = $1`,
		req.RideRequestID,
	).Scan(&driverID, &passengerID); lookupErr == nil {
		raterRole := "passenger"
		if raterID == driverID {
			raterRole = "driver"
		}
		Hub.SendToUser(req.RatedID, "rating_submitted", gin.H{
			"request_id": req.RideRequestID,
			"by_user_id": raterID,
			"by_role":    raterRole,
		})
	}

	c.JSON(http.StatusCreated, gin.H{"ok": true})
}
