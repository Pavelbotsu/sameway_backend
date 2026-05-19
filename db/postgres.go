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

func Migrate() error {
	_, err := DB.Exec(context.Background(), `
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
			updated_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_active_passengers_location
			ON active_passengers USING GIST(location);

		CREATE TABLE IF NOT EXISTS ride_requests (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			driver_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			passenger_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			pickup_location GEOGRAPHY(POINT,4326),
			status TEXT NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending','accepted','declined','cancelled')),
			created_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(driver_id, passenger_id)
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
	`)
	return err
}
