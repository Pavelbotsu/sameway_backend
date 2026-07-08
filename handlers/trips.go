package handlers

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
)

func PlanTrip(c *gin.Context) {
	// Pointer coords: `required` on plain floats rejects legal zero values
	// (equator / Greenwich meridian).
	var req struct {
		OriginName string   `json:"origin_name" binding:"required"`
		OriginLat  *float64 `json:"origin_lat" binding:"required"`
		OriginLng  *float64 `json:"origin_lng" binding:"required"`
		DestName   string   `json:"dest_name" binding:"required"`
		DestLat    *float64 `json:"dest_lat" binding:"required"`
		DestLng    *float64 `json:"dest_lng" binding:"required"`
		Seats      int      `json:"seats" binding:"required,min=1,max=6"`
		CorridorKm float64  `json:"corridor_km" binding:"max=50"`
		PlannedAt  string   `json:"planned_at" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validLatLng(*req.OriginLat, *req.OriginLng) || !validLatLng(*req.DestLat, *req.DestLng) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coordinates out of range"})
		return
	}
	if _, err := time.Parse(time.RFC3339, req.PlannedAt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "planned_at must be an RFC3339 timestamp"})
		return
	}
	if req.CorridorKm <= 0 {
		req.CorridorKm = 2.0
	}

	driverID := c.GetString("user_id")

	route, err := services.GetRoadRoute(*req.OriginLat, *req.OriginLng, *req.DestLat, *req.DestLng)
	if err != nil {
		route = &services.RouteResult{WKT: "", DistanceKm: 0}
	}

	id, err := services.CreatePlannedTrip(
		driverID, req.OriginName, *req.OriginLat, *req.OriginLng,
		req.DestName, *req.DestLat, *req.DestLng,
		req.CorridorKm, route.DistanceKm, req.Seats, req.PlannedAt, route.WKT,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "distance_km": route.DistanceKm})
}

func GetMyPlannedTrips(c *gin.Context) {
	driverID := c.GetString("user_id")
	trips, err := services.GetDriverPlannedTrips(driverID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if trips == nil {
		trips = []models.PlannedTrip{}
	}
	c.JSON(http.StatusOK, trips)
}

func CancelPlannedTrip(c *gin.Context) {
	tripID := c.Param("id")
	driverID := c.GetString("user_id")
	if err := services.DeletePlannedTrip(tripID, driverID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func SearchTrips(c *gin.Context) {
	var req struct {
		OriginLat    *float64 `json:"origin_lat" binding:"required"`
		OriginLng    *float64 `json:"origin_lng" binding:"required"`
		DestLat      float64  `json:"dest_lat"`
		DestLng      float64  `json:"dest_lng"`
		RadiusKm     float64  `json:"radius_km" binding:"max=500"`
		SortBy       string   `json:"sort_by" binding:"omitempty,oneof=distance destination time"`
		PlannedAfter string   `json:"planned_after"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validLatLng(*req.OriginLat, *req.OriginLng) || !validLatLng(req.DestLat, req.DestLng) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "coordinates out of range"})
		return
	}
	if req.RadiusKm <= 0 {
		req.RadiusKm = 50
	}

	trips, err := services.SearchPlannedTrips(
		*req.OriginLat, *req.OriginLng,
		req.DestLat, req.DestLng,
		req.RadiusKm, req.SortBy, req.PlannedAfter,
	)
	if err != nil {
		if strings.Contains(err.Error(), "planned_after") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "planned_after must be an RFC3339 timestamp"})
			return
		}
		log.Printf("SearchTrips: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if trips == nil {
		trips = []models.PlannedTrip{}
	}
	c.JSON(http.StatusOK, trips)
}
