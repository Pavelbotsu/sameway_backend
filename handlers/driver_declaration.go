package handlers

import (
	"context"
	"log"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/gin-gonic/gin"
)

// AcceptDriverDeclaration records the driver's own declaration, the same
// model BlaBlaCar uses (Terms art. 4.1 / 8.2): the platform does not check
// documents, the driver declares them and must show them on request.
// POST /auth/driver-declaration.
func AcceptDriverDeclaration(c *gin.Context) {
	var req struct {
		Licence     bool `json:"licence"`
		Insurance   bool `json:"insurance"`
		Roadworthy  bool `json:"roadworthy"`
		CostSharing bool `json:"cost_sharing"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !req.Licence || !req.Insurance || !req.Roadworthy || !req.CostSharing {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "all four statements must be confirmed to drive",
			"code":  "driver_declaration_required",
		})
		return
	}
	userID := c.GetString("user_id")
	if _, err := db.DB.Exec(c.Request.Context(), `
		UPDATE users SET driver_declared_at = NOW(), driver_declaration_version = $2
		WHERE id = $1 AND deleted_at IS NULL`,
		userID, models.CurrentDriverDeclarationVersion,
	); err != nil {
		log.Printf("AcceptDriverDeclaration %s: %v", userID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "version": models.CurrentDriverDeclarationVersion})
}

// driverReady checks what a driver needs before publishing a route or a
// planned trip: the current declaration and complete car details (the
// passenger uses them to recognise the car). It writes the error response
// and returns false when something is missing.
func driverReady(ctx context.Context, c *gin.Context, userID string) bool {
	var declared, carOK bool
	err := db.DB.QueryRow(ctx, `
		SELECT COALESCE(driver_declaration_version, 0) >= $2,
		       COALESCE(car_make,'') <> '' AND COALESCE(car_model,'') <> ''
		       AND COALESCE(car_color,'') <> '' AND COALESCE(car_plate,'') <> ''
		FROM users WHERE id = $1`, userID, models.CurrentDriverDeclarationVersion,
	).Scan(&declared, &carOK)
	switch {
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return false
	case !declared:
		c.JSON(http.StatusForbidden, gin.H{
			"error": "confirm your driving licence, insurance and car first",
			"code":  "driver_declaration_required",
		})
		return false
	case !carOK:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "add your car's make, model, colour and plate first",
			"code":  "car_details_required",
		})
		return false
	}
	return true
}
