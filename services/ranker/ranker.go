// Package ranker provides a pluggable scoring layer on top of the geometric
// PostGIS match query in services/geo.go. The default implementation is a
// passthrough (GeometricRanker) that preserves the SQL ordering. An optional
// AI implementation can re-order candidates by softer signals (rating, walk
// vs. detour trade-off, etc.).
//
// The whole AI subsystem is deletable: drop ai.go, remove the AIRankerEnabled
// branch from Init, and the application still compiles and behaves like
// today.
package ranker

import (
	"context"

	"github.com/Pavelbotsu/sameway_backend/config"
)

// Candidate is a feature-only view of a corridor match. It intentionally
// omits coordinates so that an AI ranker can be fed without leaking exact
// locations to a third-party API.
type Candidate struct {
	UserID          string
	Name            string
	AvgRating       float64
	RatingCount     int
	WalkM           float64
	DriverToPickupM float64
}

// RankContext carries ride-level context (corridor width, seat count) that
// helps a ranker reason about trade-offs without leaking driver coordinates.
type RankContext struct {
	DriverID   string
	CorridorKm float64
	Seats      int
}

// Result is the ranker's verdict for a single candidate. Score is in [0, 1];
// higher means a better match. The caller reorders the original match list
// by descending Score.
type Result struct {
	UserID string
	Score  float64
	Reason string
}

// MatchRanker is the integration boundary. Two implementations live in this
// package today; a third could be added (e.g. ML model running locally)
// without touching any handler code.
type MatchRanker interface {
	Name() string
	Rank(ctx context.Context, candidates []Candidate, rc RankContext) ([]Result, error)
}

// Active is the process-wide ranker. Handlers read this. Init may swap it
// for an AIRanker at startup; on per-call error the caller falls back to a
// fresh GeometricRanker (see handlers/driver.go notifyCorridorMatches).
var Active MatchRanker = &GeometricRanker{}

// Init wires the AI ranker when the config opts in. Safe to call multiple
// times. To remove AI entirely: delete the if branch + delete ai.go.
func Init(cfg config.Config) {
	if cfg.AIRankerEnabled && cfg.AIProviderURL != "" {
		Active = NewAIRanker(cfg)
	}
}
