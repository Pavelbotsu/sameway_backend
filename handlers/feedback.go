package handlers

import (
	"net/http"
	"strings"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
)

// SubmitBugReport stores an in-app bug report. Auth required (the user_id
// comes from JWT, not the request body). Title/body are required; device_info
// and app_version are optional metadata the Flutter client fills in.
//
// We deliberately keep this lightweight — no email forwarding, no Slack
// webhook. A separate operator process can poll the table or add a fanout
// later without touching the route.
func SubmitBugReport(c *gin.Context) {
	var req struct {
		Title      string `json:"title"`
		Body       string `json:"body"`
		AppVersion string `json:"app_version"`
		DeviceInfo string `json:"device_info"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" || body == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "title and body are required"})
		return
	}
	// Hard caps so a misbehaving client can't blow up the table.
	//
	// Truncate by RUNES, not bytes. len() counts bytes, and Cyrillic is 2
	// bytes per character (emoji 4) — the app ships full Ukrainian
	// localization, so a byte slice regularly landed mid-character. That
	// produces invalid UTF-8, which Postgres rejects outright ("invalid byte
	// sequence for encoding UTF8"), turning a long bug report into a 500.
	title = truncateRunes(title, 200)
	body = truncateRunes(body, 4000)
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(c.Request.Context(), `
		INSERT INTO bug_reports (user_id, title, body, app_version, device_info)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
	`, userID, title, body, req.AppVersion, req.DeviceInfo)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
