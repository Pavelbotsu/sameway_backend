package handlers

import (
	"context"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// pickupStillAhead recomputes, inside an open transaction, how far ahead of the
// driver's FRESH current position the request's pickup still is (along-route
// meters). Returns ok=false when it can't be determined — no route, no/stale
// location — in which case the caller should allow the accept (we can't prove
// the driver passed).
//
// This is the accept-time guard against the stale-pending race: between match
// and accept the driver may have driven past the pickup on a fast road. Callers
// reject when ahead < MatchMinAheadM and mark the row expired_passed.
func pickupStillAhead(ctx context.Context, tx pgx.Tx, requestID string) (aheadM float64, ok bool) {
	// 3857, not raw 4326: planar maths on degrees is anisotropic, which shifts
	// both projections along the route. Here that can flip the SIGN — the
	// difference between "the driver is still approaching" and "the driver has
	// passed you" — so it must match the corridor query's projection exactly.
	err := tx.QueryRow(ctx, `
		SELECT SIGN(p.pf - p.df) * ST_Length(ST_Transform(ST_LineSubstring(
		           rp.route_p, LEAST(p.df, p.pf), GREATEST(p.df, p.pf)
		       ), 4326)::geography)
		FROM ride_requests rr
		JOIN active_drivers ad ON ad.driver_id = rr.driver_id
		CROSS JOIN LATERAL (
		    SELECT ad.route_3857 AS route_p
		) rp
		CROSS JOIN LATERAL (
		    SELECT ST_LineLocatePoint(rp.route_p, ST_Transform(rr.pickup_location::geometry, 3857)) AS pf,
		           ST_LineLocatePoint(rp.route_p, ST_Transform(ad.current_location::geometry, 3857)) AS df
		) p
		WHERE rr.id = $1
		  AND ad.route IS NOT NULL
		  AND ad.current_location IS NOT NULL
		  AND ad.current_location_at > NOW() - make_interval(secs => $2::float8)
	`, requestID, config.C.MatchLocFreshS).Scan(&aheadM)
	if err != nil {
		// ErrNoRows (stale/missing location or route) → can't prove passed.
		return 0, false
	}
	return aheadM, true
}

// expirePassedRide marks a request expired_passed within the open tx, commits,
// and notifies the passenger so their UI frees the slot ("driver already
// passed, finding another"). Reuses the ride_cancelled event the client already
// handles, tagged cancelled_by="driver_passed". Returns true on success.
func expirePassedRide(ctx context.Context, tx pgx.Tx, requestID, passengerID string) bool {
	if _, err := tx.Exec(ctx,
		`UPDATE ride_requests SET status = 'expired_passed' WHERE id = $1`, requestID,
	); err != nil {
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		return false
	}
	Hub.SendToUser(passengerID, "ride_cancelled", gin.H{
		"request_id":   requestID,
		"cancelled_by": "driver_passed",
	})
	go safeGo("match-event", func() {
		logMatchEvent(requestID, "expired", map[string]any{"why": "driver_passed"})
	})()
	return true
}
