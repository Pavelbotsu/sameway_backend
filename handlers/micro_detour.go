package handlers

import (
	"context"
	"log"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/services"
)

// Micro-detour pass (Round 2 §2): the ONE narrow exception to the zero-detour
// invariant. Runs only as a fallback after a corridor run, only for drivers
// who opted in (users.allow_micro_detour), and only over candidates that
// failed corridor matching SOLELY on walk distance. Each accepted candidate
// costs one OSRM via-route call; the pass is bounded by
// MatchDetourMaxCandidates and both per-candidate caps AND a cumulative
// per-route budget. The delta is disclosed to both sides — never silent.
func runMicroDetourPass(driverID string) {
	ctx := context.Background()

	var allow bool
	if err := db.DB.QueryRow(ctx,
		`SELECT allow_micro_detour FROM users WHERE id = $1`, driverID,
	).Scan(&allow); err != nil || !allow {
		return
	}

	// Driver state — the pass needs a FRESH current position (same freshness
	// gate as matching): the comparison baseline is current→destination, and
	// a stale fix would make the delta meaningless. Stale → skip entirely.
	var (
		curLat, curLng, destLat, destLng, corridorKm float64
		seats, taken                                 int
		routeWKT                                     string
	)
	err := db.DB.QueryRow(ctx, `
		SELECT ST_Y(current_location::geometry), ST_X(current_location::geometry),
		       ST_Y(destination::geometry), ST_X(destination::geometry),
		       corridor_km, seats, seats_taken, COALESCE(ST_AsText(route), '')
		FROM active_drivers
		WHERE driver_id = $1 AND status = 'looking'
		  AND current_location IS NOT NULL
		  AND current_location_at > NOW() - make_interval(secs => $2::float8)
	`, driverID, config.C.MatchLocFreshS).Scan(
		&curLat, &curLng, &destLat, &destLng, &corridorKm, &seats, &taken, &routeWKT)
	if err != nil || routeWKT == "" || taken >= seats {
		return
	}

	cands, err := services.FindNearMissCandidates(driverID, routeWKT, corridorKm, config.C.MatchDetourMaxCandidates)
	if err != nil {
		log.Printf("micro-detour %s: %v", driverID, err)
		return
	}
	if len(cands) == 0 {
		return
	}

	// Fresh baseline, ONCE per pass, from the current position — NEVER the
	// origin-anchored route stored at SetRoute time (mid-trip that baseline
	// is stale and the delta would be wrong).
	base, err := services.GetRoute("driving", curLat, curLng, destLat, destLng)
	if err != nil {
		return
	}

	// Cumulative budget: live micro_detour engagements count (offered too —
	// several outstanding offers must not each pass individually and blow
	// the budget once accepted).
	//
	// Known accepted tradeoff (deliberate, not fixed this round): two
	// concurrent detour passes for the same driver — e.g. a movement rerun
	// and the passenger-appear trigger firing close together — can each read
	// this sum before either writes, slightly overrunning the cumulative
	// total. That affects only this disclosed comfort budget, never seat
	// safety or the accept-time transaction, so no lock is taken here.
	var sumM, sumS float64
	_ = db.DB.QueryRow(ctx, `
		SELECT COALESCE(SUM(detour_added_m), 0), COALESCE(SUM(detour_added_s), 0)
		FROM ride_requests
		WHERE driver_id = $1 AND match_kind = 'micro_detour'
		  AND status IN ('offered','pending','accepted','in_progress')
	`, driverID).Scan(&sumM, &sumS)

	// Sequential in-pass accounting: each accepted offer's delta joins the
	// running total BEFORE the next candidate is evaluated — a batch
	// snapshot would let several individually-passing candidates be offered
	// together and silently exceed the budget.
	for _, cand := range cands {
		via, err := services.GetRouteVia("driving",
			[2]float64{curLat, curLng},
			[2]float64{cand.PaxLat, cand.PaxLng},
			[2]float64{destLat, destLng},
		)
		if err != nil {
			continue
		}
		addM := via.DistanceKm*1000 - base.DistanceKm*1000
		addS := via.DurationS - base.DurationS
		if addM < 0 {
			addM = 0
		}
		if addS < 0 {
			addS = 0
		}
		// Both per-candidate caps must hold…
		if addM > config.C.MatchMaxDetourM || addS > config.C.MatchMaxDetourS {
			continue
		}
		// …and the route-level budget after earlier same-pass offers.
		if sumM+addM > config.C.MatchMaxDetourTotalM || sumS+addS > config.C.MatchMaxDetourTotalS {
			continue
		}

		match := services.CorridorMatch{
			User: cand.User,
			// The detour brings the car to the passenger: pickup is their
			// raw location and the walk disappears.
			PickupLat:    cand.PaxLat,
			PickupLng:    cand.PaxLng,
			WalkM:        0,
			EtaPickupS:   cand.EtaPickupS,
			CarryM:       cand.CarryM,
			MatchKind:    "micro_detour",
			DetourAddedM: addM,
			DetourAddedS: addS,
		}
		if n := notifyCorridorMatches(driverID, []services.CorridorMatch{match},
			destLat, destLng, corridorKm, seats, false); n > 0 {
			sumM += addM
			sumS += addS
			emitCorridorStats(driverID, 1, n)
		}
	}
}
