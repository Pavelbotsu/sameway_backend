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
		-- Phase 1.3 (UX readiness): profile photo URL. Hosted on Firebase
		-- Storage; NULL/empty means use initials fallback in the UI.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS photo_url TEXT;
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
		-- NOTE: ride_requests_status_check is defined ONCE, further down, with
		-- the full status set. It used to be dropped and re-added here as well,
		-- with a narrower set that the later block immediately replaced — two
		-- extra ACCESS EXCLUSIVE locks and two full table revalidations on
		-- every single boot, for a constraint that never survived the script.

		DROP INDEX IF EXISTS uniq_active_ride_request;
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_active_ride_request
			ON ride_requests (driver_id, passenger_id)
			WHERE status IN ('offered','pending','accepted','in_progress');

		-- Bug reports: in-app feedback channel. Read with psql; future
		-- versions may forward to email/Slack.
		CREATE TABLE IF NOT EXISTS bug_reports (
			id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title       TEXT NOT NULL,
			body        TEXT NOT NULL,
			app_version TEXT,
			device_info TEXT,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS bug_reports_created_at_idx
			ON bug_reports (created_at DESC);

		-- Password reset codes. Keyed by email so we can return
		-- "if an account exists, a code was sent" without leaking enumeration.
		-- Rows are short-lived (15 minute expiry); cleanup is on next request
		-- for the same email or via periodic delete (left for ops).
		CREATE TABLE IF NOT EXISTS password_resets (
			email      TEXT PRIMARY KEY,
			code       CHAR(6) NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- Phase 2.3 (UX readiness): message read receipts. NULL until the
		-- recipient's chat screen marks the message read; once stamped, the
		-- sender's UI flips ✓ → ✓✓.
		ALTER TABLE messages ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ;

		-- Phase 3.2 (UX readiness): identity-verified flag. Always FALSE
		-- until a future ID-verification flow flips it. The UI gates the
		-- check-overlay on TRUE only, so unverified users never see a
		-- "not verified" negative signal.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT FALSE;

		-- Phase 3.3 (UX readiness): profile-completion fields.
		--   bio              — one-line public bio (160 char cap enforced
		--                      at the API layer).
		--   phone            — E.164-ish raw text; we don't enforce a strict
		--                      format here so the user can type "+44 7700…"
		--                      with spaces. Future Twilio integration would
		--                      normalize on its way in.
		--   email_verified_at — non-null once the user confirms the code.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS bio TEXT NOT NULL DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT NOT NULL DEFAULT '';
		ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;

		-- Outstanding email-verification codes. Same shape as
		-- password_resets so the email infra is reused 1:1. Rows are
		-- short-lived (15 minute expiry) and cleared on confirm.
		CREATE TABLE IF NOT EXISTS email_verifications (
			user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			code       CHAR(6) NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- ── Matching rework (ETA-window, no-passed-by, same-way) ──────────────
		-- Freshness timestamp for current_location: lets the corridor match and
		-- accept re-validation skip drivers with stale/unknown position instead
		-- of falling back to origin (which made the whole route read as "ahead").
		ALTER TABLE active_drivers ADD COLUMN IF NOT EXISTS current_location_at TIMESTAMPTZ;

		-- Measured copy of the route: a LINESTRING M whose M value at each
		-- vertex is the cumulative OSRM travel time in seconds. ST_InterpolatePoint
		-- then yields drive-ETA to any projected point with no per-candidate OSRM
		-- call. Plain geometry (not geography) because measures need geometry.
		ALTER TABLE active_drivers ADD COLUMN IF NOT EXISTS route_m geometry(LineStringM,4326);

		-- Phase 2: per-vertex OSM road class (from OSRM steps) for
		-- stoppability + city/fast carry split.
		-- ALTER TABLE active_drivers ADD COLUMN IF NOT EXISTS route_classes TEXT[];

		-- Opt-in longer walk: default matching uses a short walk cap; a
		-- passenger willing to walk further for a same-direction ride sets this.
		ALTER TABLE active_passengers ADD COLUMN IF NOT EXISTS allow_long_walk BOOLEAN NOT NULL DEFAULT FALSE;

		-- New terminal status: driver provably passed the pickup before the
		-- ride went active. Outside the active set in uniq_active_ride_request,
		-- so it frees the passenger's match slot immediately.
		--
		-- 'offered' = a driver-broadcast corridor offer awaiting the passenger's
		-- accept. The passenger's accept promotes it to 'pending' (awaiting the
		-- driver's confirm); the driver finalizes to 'accepted'. So the driver
		-- always decides who rides. Note the index is per (driver, passenger)
		-- PAIR: a passenger may hold offers from several drivers at once (they
		-- pick), but never two active rows with the same driver. Committed
		-- engagements (pending/accepted/in_progress) remove the passenger from
		-- corridor candidacy via the NOT EXISTS in FindPassengersInCorridor.
		ALTER TABLE ride_requests DROP CONSTRAINT IF EXISTS ride_requests_status_check;
		ALTER TABLE ride_requests ADD CONSTRAINT ride_requests_status_check
			CHECK (status IN ('offered','pending','accepted','in_progress','done','declined','cancelled','expired_passed'));

		-- ── Release hardening ─────────────────────────────────────────────────
		-- Brute-force cap on reset / verification codes: the row dies after
		-- too many wrong guesses, not just on TTL.
		ALTER TABLE password_resets ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;
		ALTER TABLE email_verifications ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;

		-- Mid-ride safety check-in: when a rider answers "no, something's
		-- wrong" to the divergence prompt we stamp the ride so it can be
		-- reviewed afterwards. This is a REVIEW RECORD ONLY -- nothing pages
		-- anyone; the actionable escalation is the SOS sheet the user gets on
		-- screen immediately. The partial index keeps the review query cheap
		-- while costing nothing on the overwhelmingly common NULL rows.
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS safety_flagged_at TIMESTAMPTZ;
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS safety_flagged_by UUID REFERENCES users(id);
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS safety_flag_reason TEXT;
		CREATE INDEX IF NOT EXISTS idx_ride_requests_safety_flagged
			ON ride_requests (safety_flagged_at) WHERE safety_flagged_at IS NOT NULL;

		-- Status-scan indexes: the reaper sweeps ride_requests every 60 s and
		-- every request-list endpoint filters on (party, status); the partial
		-- unique index only covers active-status pairs.
		CREATE INDEX IF NOT EXISTS idx_ride_requests_driver_status
			ON ride_requests (driver_id, status);
		CREATE INDEX IF NOT EXISTS idx_ride_requests_passenger_status
			ON ride_requests (passenger_id, status);
		CREATE INDEX IF NOT EXISTS idx_ride_requests_status_created
			ON ride_requests (status, created_at);

		-- Rating aggregates are recomputed on every profile load / match
		-- notify; trip history filters by either party.
		CREATE INDEX IF NOT EXISTS idx_ratings_rated ON ratings (rated_id);
		CREATE INDEX IF NOT EXISTS idx_completed_trips_driver ON completed_trips (driver_id);
		CREATE INDEX IF NOT EXISTS idx_completed_trips_passenger ON completed_trips (passenger_id);

		-- One completed_trips row per ride: gives RecordCompletedTrip's
		-- ON CONFLICT DO NOTHING an actual arbiter (previously there was no
		-- unique constraint, so double-submits duplicated CO2/km stats).
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_completed_trips_request
			ON completed_trips (ride_request_id)
			WHERE ride_request_id IS NOT NULL;

		-- ── Round 2: persistent pair-keyed chat ───────────────────────────────
		-- One conversation per unordered user pair, independent of any single
		-- ride_request. Canonical order (user_a < user_b) is the writers'
		-- responsibility — every INSERT/lookup goes through LEAST/GREATEST;
		-- the CHECK only rejects rows that skipped that path.
		CREATE TABLE IF NOT EXISTS conversations (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_a UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			user_b UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_message_at TIMESTAMPTZ,
			CHECK (user_a < user_b)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_conversation_pair
			ON conversations (user_a, user_b);
		CREATE INDEX IF NOT EXISTS idx_conversations_last_message
			ON conversations (last_message_at DESC NULLS LAST);

		-- Messages move from ride_request-keyed to conversation-keyed.
		-- ride_request_id stays (nullable) for the one-release compat shim and
		-- as provenance on historical rows.
		ALTER TABLE messages ADD COLUMN IF NOT EXISTS conversation_id UUID REFERENCES conversations(id) ON DELETE CASCADE;
		ALTER TABLE messages ALTER COLUMN ride_request_id DROP NOT NULL;
		CREATE INDEX IF NOT EXISTS idx_messages_conversation
			ON messages (conversation_id, created_at);

		-- Backfill: mint conversations for every historical message pair, then
		-- stamp conversation_id on their messages. Both statements are
		-- idempotent (ON CONFLICT / conversation_id IS NULL guard).
		-- Self-pairs (driver_id = passenger_id — degenerate rows from before
		-- the self-request guard existed) are skipped: LEAST = GREATEST there
		-- and the strict user_a < user_b check would reject the row. Their
		-- messages simply keep ride_request_id-only keying.
		INSERT INTO conversations (user_a, user_b)
		SELECT DISTINCT LEAST(rr.driver_id, rr.passenger_id),
		                GREATEST(rr.driver_id, rr.passenger_id)
		FROM messages m
		JOIN ride_requests rr ON rr.id = m.ride_request_id
		WHERE m.conversation_id IS NULL
		  AND rr.driver_id <> rr.passenger_id
		ON CONFLICT (user_a, user_b) DO NOTHING;

		UPDATE messages m
		SET conversation_id = c.id
		FROM ride_requests rr, conversations c
		WHERE rr.id = m.ride_request_id
		  AND c.user_a = LEAST(rr.driver_id, rr.passenger_id)
		  AND c.user_b = GREATEST(rr.driver_id, rr.passenger_id)
		  AND m.conversation_id IS NULL;

		-- Abuse controls: blocks stop messaging in both directions; reports
		-- are an audit trail for ops (no automated action yet).
		CREATE TABLE IF NOT EXISTS user_blocks (
			blocker_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			blocked_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (blocker_id, blocked_id)
		);
		CREATE TABLE IF NOT EXISTS user_reports (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			reporter_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			reported_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			conversation_id UUID REFERENCES conversations(id) ON DELETE SET NULL,
			reason TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- ── Round 2: labeled match kinds + outcome log ────────────────────────
		-- match_kind labels HOW a request was matched: 'corridor' (round-1
		-- push), 'corridor_weak' (no destination), 'partial_relay' (ride to
		-- the route end, continue by transit/second leg), 'micro_detour'
		-- (driver opted into a small detour). dropoff_location is set for
		-- partial_relay (the route end the passenger rides to).
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS match_kind TEXT NOT NULL DEFAULT 'corridor';
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS dropoff_location GEOGRAPHY(POINT,4326);
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS detour_added_m DOUBLE PRECISION;
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS detour_added_s DOUBLE PRECISION;

		-- Static GTFS stops (transit awareness for partial-relay dropoffs).
		-- Populated by the boot-time loader from GTFS_STATIC_URL; empty table
		-- simply disables the transit branch of the feasibility check.
		CREATE TABLE IF NOT EXISTS transit_stops (
			stop_id     TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			location    GEOGRAPHY(POINT,4326) NOT NULL,
			imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_transit_stops_location
			ON transit_stops USING GIST (location);

		-- Append-only outcome log for partial_relay / micro_detour matches —
		-- the labeled training data for a future ranking model. Nothing
		-- consumes it yet; do not join it into hot paths.
		CREATE TABLE IF NOT EXISTS match_events (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id UUID NOT NULL REFERENCES ride_requests(id) ON DELETE CASCADE,
			match_kind TEXT NOT NULL,
			event      TEXT NOT NULL,
			meta       JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_match_events_request
			ON match_events (request_id, created_at);

		-- Micro-detour opt-in (driver-level, drives server-side matching).
		ALTER TABLE users ADD COLUMN IF NOT EXISTS allow_micro_detour BOOLEAN NOT NULL DEFAULT FALSE;

		-- ── Session revocation ────────────────────────────────────────────────
		-- Stamped into every JWT as the "tv" claim and re-checked on each
		-- authenticated request. Incrementing it invalidates all outstanding
		-- tokens for that user. Needed because the tokens are stateless: before
		-- this, changing a compromised account's password left the attacker's
		-- token working for its full lifetime, and a role switch left the
		-- pre-switch token usable against the old role's endpoints.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 0;

		-- Brute-force cap for the pickup handshake. The 4-digit code is the
		-- only evidence that driver and passenger physically met, so an
		-- uncapped guess loop defeated the entire control.
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS pickup_attempts INT NOT NULL DEFAULT 0;

		-- Reaper input. Offers age out on created_at, but an offer promoted to
		-- 'pending' (passenger accepted, awaiting the driver's confirm) must get
		-- a FRESH window — otherwise a passenger accepting near the end of the
		-- offer timeout left the driver only the few remaining seconds to
		-- confirm before the row was cancelled underneath them.
		ALTER TABLE ride_requests ADD COLUMN IF NOT EXISTS status_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

		-- Maintained by trigger rather than by hand. There are ~18 sites that
		-- write ride_requests.status across handlers and services; stamping the
		-- column at each of them would silently rot the moment someone adds the
		-- nineteenth. The trigger cannot be forgotten.
		CREATE OR REPLACE FUNCTION ride_requests_stamp_status_change() RETURNS trigger AS $$
		BEGIN
			IF NEW.status IS DISTINCT FROM OLD.status THEN
				NEW.status_changed_at := NOW();
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS trg_ride_requests_status_changed ON ride_requests;
		CREATE TRIGGER trg_ride_requests_status_changed
			BEFORE UPDATE ON ride_requests
			FOR EACH ROW EXECUTE FUNCTION ride_requests_stamp_status_change();

		-- The reaper sweeps on status_changed_at now, not created_at.
		CREATE INDEX IF NOT EXISTS idx_ride_requests_status_changed
			ON ride_requests (status, status_changed_at);

		-- ── Legal consent + account deletion ─────────────────────────────────
		-- terms_version: the Terms/Privacy version the user last accepted
		-- (0 = never; handlers.CurrentTermsVersion is the live one, and bumping
		-- it makes every client re-prompt). age_confirmed_at: the 18+
		-- attestation. Both are recorded server-side because the stores and
		-- GDPR both expect the operator to be able to show WHEN consent was
		-- given, not just that the app has a checkbox.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS terms_version INT NOT NULL DEFAULT 0;
		ALTER TABLE users ADD COLUMN IF NOT EXISTS terms_accepted_at TIMESTAMPTZ;
		ALTER TABLE users ADD COLUMN IF NOT EXISTS age_confirmed_at TIMESTAMPTZ;
		-- Set when the account is deleted. The row itself is kept but scrubbed
		-- of every personal field (see handlers.deleteAccount), so the
		-- counterparty's ride history, ratings and safety reports keep their
		-- foreign keys instead of cascading away.
		ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
`
