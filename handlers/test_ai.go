package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/services/ranker"
	"github.com/gin-gonic/gin"
)

// TestAIRanker fires a synthetic ranking request through `ranker.Active` so
// the user can verify their AI provider is reachable without setting up a
// full route. Returns the ranker name, latency, the top result, and whether
// AI was actually used (vs the geometric fallback).
//
// Lives under `/auth/test-ai` (auth required, no role gate). Cheap to call —
// 3 synthetic candidates, 2-second hard timeout matches the production
// rank() timeout.
func TestAIRanker(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	cands := []ranker.Candidate{
		{UserID: "p-test-a", WalkM: 120, DriverToPickupM: 800, AvgRating: 4.8, RatingCount: 42},
		{UserID: "p-test-b", WalkM: 540, DriverToPickupM: 350, AvgRating: 4.1, RatingCount: 6},
		{UserID: "p-test-c", WalkM: 80, DriverToPickupM: 1500, AvgRating: 4.9, RatingCount: 130},
	}
	rc := ranker.RankContext{CorridorKm: 2.0, Seats: 3}

	configured := config.C.AIRankerEnabled &&
		config.C.AIProviderURL != "" &&
		config.C.AIProviderAPIKey != "" &&
		config.C.AIModelName != ""

	chosen := ranker.Active
	start := time.Now()
	results, err := chosen.Rank(ctx, cands, rc)
	latency := time.Since(start)

	resp := gin.H{
		"ai_configured": configured,
		"ranker_name":   chosen.Name(),
		"latency_ms":    latency.Milliseconds(),
		"model_name":    config.C.AIModelName,
	}

	if err != nil {
		resp["ok"] = false
		resp["error"] = err.Error()
		resp["used_fallback"] = true
		c.JSON(http.StatusOK, resp)
		return
	}

	resp["ok"] = true
	resp["used_fallback"] = !strings.HasPrefix(chosen.Name(), "ai-")
	if len(results) > 0 {
		top := results[0]
		resp["top_user_id"] = top.UserID
		resp["top_score"] = top.Score
		resp["top_reason"] = top.Reason
	}
	c.JSON(http.StatusOK, resp)
}
