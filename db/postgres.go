package db

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

var DB *pgxpool.Pool

func Connect() error {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		config.C.DBHost, config.C.DBPort, config.C.DBUser, config.C.DBPassword, config.C.DBName,
	)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	DB = pool
	return Migrate()
}

// Migrate runs the schema migration against the global pool. Existing
// production call site in main.go is unaffected.
func Migrate() error {
	return MigratePool(context.Background(), DB)
}

// MigratePool runs the same schema migration against an arbitrary pool. Used
// by tests to set up a fresh container-backed database.
func MigratePool(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, alterSQL)
	return err
}

const schemaSQL = `
		CREATE EXTENSION IF NOT EXISTS postgis;

		CREATE TABLE IF NOT EXISTS users (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name TEXT NOT NULL,
			email TEXT UNIQUE NOT NULL,
			password TEXT NOT NULL,
			role TEXT NOT NULL CHECK (role IN ('driver','passenger')),
			fcm_token TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS active_drivers (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			driver_id UUID UNIQUE NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			origin GEOGRAPHY(POINT,4326) NOT NULL,
			destination GEOGRAPHY(POINT,4326) NOT NULL,
			route GEOGRAPHY(LINESTRING,4326),
			current_location GEOGRAPHY(POINT,4326),
			corridor_km FLOAT NOT NULL DEFAULT 1.0,
			seats INT NOT NULL DEFAULT 3,
			status TEXT NOT NULL DEFAULT 'looking'
				CHECK (status IN ('looking','full','in_progress','done')),
			created_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_active_drivers_route
			ON active_drivers USING GIST(route);

		CREATE TABLE IF NOT EXISTS active_passengers (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			passenger_id UUID UNIQUE NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			location GEOGRAPHY(POINT,4326) NOT NULL,
			destination_lat DOUBLE PRECISION,
			destination_lng DOUBLE PRECISION,
			updated_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_active_passengers_location
			ON active_passengers USING GIST(location);

		CREATE TABLE IF NOT EXISTS ride_requests (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			driver_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			passenger_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			pickup_location GEOGRAPHY(POINT,4326),
			pickup_code CHAR(4),
			picked_up_at TIMESTAMPTZ,
			status TEXT NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending','accepted','in_progress','done','declined','cancelled')),
			created_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS completed_trips (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			ride_request_id UUID REFERENCES ride_requests(id) ON DELETE SET NULL,
			driver_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			passenger_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			distance_km FLOAT NOT NULL DEFAULT 0,
			co2_saved_kg FLOAT NOT NULL DEFAULT 0,
			completed_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS messages (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			ride_request_id UUID NOT NULL REFERENCES ride_requests(id) ON DELETE CASCADE,
			sender_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			content TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_messages_request
			ON messages (ride_request_id, created_at);

		CREATE TABLE IF NOT EXISTS planned_trips (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			driver_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			origin_name TEXT NOT NULL,
			origin GEOGRAPHY(POINT,4326) NOT NULL,
			destination_name TEXT NOT NULL,
			destination GEOGRAPHY(POINT,4326) NOT NULL,
			route GEOGRAPHY(LINESTRING,4326),
			seats INT NOT NULL DEFAULT 1,
			corridor_km FLOAT NOT NULL DEFAULT 2.0,
			distance_km FLOAT NOT NULL DEFAULT 0,
			planned_at TIMESTAMPTZ NOT NULL,
			status TEXT NOT NULL DEFAULT 'open'
				CHECK (status IN ('open','full','cancelled','done')),
			created_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_planned_trips_origin
			ON planned_trips USING GIST(origin);
		CREATE INDEX IF NOT EXISTS idx_planned_trips_destination
			ON planned_trips USING GIST(destination);
		CREATE INDEX IF NOT EXISTS idx_planned_trips_planned_at
			ON planned_trips (planned_at);

		CREATE TABLE IF NOT EXISTS ratings (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			rater_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			rated_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			ride_request_id UUID NOT NULL REFERENCES ride_requests(id) ON DELETE CASCADE,
			stars SMALLINT NOT NULL CHECK (stars BETWEEN 1 AND 5),
			comment TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(rater_id, ride_request_id)
		);
`

const alterSQL = `
		ALTER TABLE users ADD COLUMN IF NOT EXISTS car_make TEXT DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS car_model TEXT DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS car_color TEXT DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS car_plate TEXT DEFAULT '';
		ALTER TABLE active_drivers ADD COLUMN IF NOT EXISTS seats_taken INT NOT NULL DEFAULT 0;
		ALTER TABLE active_passengers ADD COLUMN IF NOT EXISTS destination_lat DOUBLE PRECISION;
		ALTER TABLE active_passengers ADD COLUMN IF NOT EXISTS destination_lng DOUBLE PRECISION;
		ALTER TABLE active_drivers ADD COLUMN IF NOT EXISTS current_location GEOGRAPHY(POINT,4326);
		CREATE INDEX IF NOT EXISTS idx_active_drivers_current_location
			ON active_drivers USING GIST(current_location);

		-- Replace the absolute uniqueness on ride_requests with one scoped to
		-- active rows so cancelled/declined history coexists without blocking
		-- new matches between the same (driver, passenger) pair.
		ALTER TABLE ride_requests DROP CONSTRAINT IF EXISTS ride_requests_driver_id_passenger_id_key;

		-- Pickup-confirmation handshake (4-digit code + timestamp). Status
		-- gains 'in_progress' (after pickup) and 'done' (after dropoff). The
		-- partial unique index now also covers in_progress so a passenger
		-- can't be matched twice while in the car.
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS pickup_code CHAR(4);
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS picked_up_at TIMESTAMPTZ;
		ALTER TABLE ride_requests DROP CONSTRAINT IF EXISTS ride_requests_status_check;
		ALTER TABLE ride_requests ADD CONSTRAINT ride_requests_status_check
			CHECK (status IN ('pending','accepted','in_progress','done','declined','cancelled'));

		DROP INDEX IF EXISTS uniq_active_ride_request;
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_active_ride_request
			ON ride_requests (driver_id, passenger_id)
			WHERE status IN ('pending','accepted','in_progress');
`
