package testutil

import (
	"context"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// LatLng is a tiny lat/lng pair so seeders don't need to import latlong2 or
// build PostGIS strings inline.
type LatLng struct{ Lat, Lng float64 }

// CreateUser inserts a users row directly + mints a JWT. Returns (id, token).
// Email is derived from `name` so callers can have many users without thinking.
// Password is the bcrypted form of "test-pass".
func CreateUser(t testing.TB, pool *pgxpool.Pool, role, name string) (id, token string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("test-pass"), bcrypt.MinCost)
	require.NoError(t, err)

	err = pool.QueryRow(context.Background(), `
		INSERT INTO users (name, email, password, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, name, name+"@test.local", string(hash), role).Scan(&id)
	require.NoError(t, err, "insert user %q", name)

	tok, err := middleware.GenerateToken(id, role)
	require.NoError(t, err, "mint token")
	return id, tok
}

// CreateActiveDriver upserts an active_drivers row with an arbitrary route
// linestring. `route` should be a LINESTRING WKT (see LineStringWKT). Seats
// default to 3, corridor to 1 km — override with raw SQL if needed.
func CreateActiveDriver(t testing.TB, pool *pgxpool.Pool, driverID string, origin, dest LatLng, routeWKT string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO active_drivers (
			driver_id, origin, destination, route, current_location,
			corridor_km, seats, status
		)
		VALUES (
			$1,
			ST_MakePoint($3, $2)::geography,
			ST_MakePoint($5, $4)::geography,
			ST_GeogFromText($6),
			ST_MakePoint($3, $2)::geography,
			1.0, 3, 'looking'
		)
		ON CONFLICT (driver_id) DO UPDATE SET
			origin           = EXCLUDED.origin,
			destination      = EXCLUDED.destination,
			route            = EXCLUDED.route,
			current_location = EXCLUDED.origin,
			status           = 'looking',
			seats_taken      = 0,
			created_at       = NOW()
	`, driverID, origin.Lat, origin.Lng, dest.Lat, dest.Lng, routeWKT)
	require.NoError(t, err)
}

// SetDriverCurrentLocation updates active_drivers.current_location only —
// useful for heading-filter tests that move the driver along the route.
func SetDriverCurrentLocation(t testing.TB, pool *pgxpool.Pool, driverID string, loc LatLng) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE active_drivers
		SET current_location = ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
		WHERE driver_id = $3
	`, loc.Lng, loc.Lat, driverID)
	require.NoError(t, err)
}

// CreateActivePassenger upserts an active_passengers row. Pass an optional
// destination (zero-value LatLng means NULL → fall back to legacy match).
func CreateActivePassenger(t testing.TB, pool *pgxpool.Pool, passengerID string, loc LatLng, dest *LatLng) {
	t.Helper()
	if dest == nil {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO active_passengers (passenger_id, location, updated_at)
			VALUES ($1, ST_MakePoint($3, $2)::geography, NOW())
			ON CONFLICT (passenger_id) DO UPDATE SET
				location = EXCLUDED.location,
				updated_at = NOW(),
				destination_lat = NULL,
				destination_lng = NULL
		`, passengerID, loc.Lat, loc.Lng)
		require.NoError(t, err)
		return
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO active_passengers (passenger_id, location, destination_lat, destination_lng, updated_at)
		VALUES ($1, ST_MakePoint($3, $2)::geography, $4, $5, NOW())
		ON CONFLICT (passenger_id) DO UPDATE SET
			location = EXCLUDED.location,
			destination_lat = EXCLUDED.destination_lat,
			destination_lng = EXCLUDED.destination_lng,
			updated_at = NOW()
	`, passengerID, loc.Lat, loc.Lng, dest.Lat, dest.Lng)
	require.NoError(t, err)
}
