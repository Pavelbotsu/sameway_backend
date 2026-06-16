package ranker

import "context"

// GeometricRanker preserves the SQL ordering produced by
// FindPassengersInCorridor (driver_to_pickup_m ASC). It is the always-safe
// fallback and the default when AI is disabled.
type GeometricRanker struct{}

func (GeometricRanker) Name() string { return "geometric" }

func (GeometricRanker) Rank(_ context.Context, cands []Candidate, _ RankContext) ([]Result, error) {
	out := make([]Result, len(cands))
	for i, c := range cands {
		// Score decays with input position so the existing SQL order survives
		// a stable sort by descending Score.
		out[i] = Result{UserID: c.UserID, Score: 1.0 - float64(i)/1000.0}
	}
	return out, nil
}
