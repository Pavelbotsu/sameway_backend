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
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}
