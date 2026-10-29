package services_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Guards the CRS choice behind every projection in the matching queries.
//
// ST_LineLocatePoint / ST_ClosestPoint are planar. Applied to raw 4326 they
// treat degrees as a flat grid, but a degree of longitude covers only cos(lat)
// of the ground distance of a degree of latitude — ~0.64 at Krakow's latitude.
// On a route with both N-S and E-W components that anisotropy drags the
// "closest" point along the line and inflates the walk-to-pickup distance.
//
// The rest of the geo suite cannot catch this: its fixtures use routes of
// constant latitude (pure E-W), where the distortion is axis-aligned and
// cancels. This test deliberately uses a diagonal.
func TestProjection_IsAccurateOnDiagonalRoutes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	const line = "LINESTRING(20 50, 20.1 50.1)" // 45° in degree space; not on the ground
	const px, py = 20.08, 50.02                 // passenger off to one side

	var degreeSpaceM, projectedM, trueMinM float64
	require.NoError(t, pool.QueryRow(ctx, `
		WITH l AS (SELECT ST_GeomFromText($1, 4326) AS g),
		     p AS (SELECT ST_SetSRID(ST_MakePoint($2::float8, $3::float8), 4326) AS g)
		SELECT
		  -- what a naive 4326 projection picks
		  ST_Distance(p.g::geography, ST_ClosestPoint(l.g, p.g)::geography),
		  -- what the queries actually do: project through 3857, come back
		  ST_Distance(p.g::geography,
		      ST_Transform(ST_ClosestPoint(ST_Transform(l.g, 3857),
		                                   ST_Transform(p.g, 3857)), 4326)::geography),
		  -- ground truth
		  ST_Distance(p.g::geography, l.g::geography)
		FROM l, p
	`, line, px, py).Scan(&degreeSpaceM, &projectedM, &trueMinM))

	t.Logf("degree-space %.1f m | 3857 %.1f m | true minimum %.1f m",
		degreeSpaceM, projectedM, trueMinM)

	// The metric projection must land essentially on the true nearest point.
	assert.InDelta(t, trueMinM, projectedM, 10,
		"3857 projection should find the true nearest point")

	// And it must be a real improvement, not a wash. If this stops holding the
	// projection has silently regressed to degree space — measured at ~351 m of
	// overstated walk here, more than half the default 600 m walk cap, i.e.
	// enough to reject matches that should have been offered.
	assert.Greater(t, degreeSpaceM-trueMinM, 100.0,
		"sanity: degree-space projection should be materially worse on a diagonal")
	assert.Less(t, projectedM, degreeSpaceM,
		"metric projection must beat degree space")
}
