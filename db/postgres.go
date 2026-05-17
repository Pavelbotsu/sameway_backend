package db

import (
	"context"
	"fmt"

	"github.com/Pavelbotsu/rideshare-backend/config"
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
	`)
	return err
}
