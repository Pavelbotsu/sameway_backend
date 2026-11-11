package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	// AppEnv is "dev" or "prod". Prod refuses to boot on insecure defaults
	// (see Validate).
	AppEnv     string
	Port       string
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	// DB pool: max connections and per-statement timeout (ms). See
	// db.ConfigurePool for why both are set explicitly.
	DBMaxConns                int
	DBStatementTimeoutMs      int
	JWTSecret                 string
	JWTTTLHours               int
	FirebaseCredPath          string
	OSRMBaseURL               string
	MaxCorridorNotifications  int
	RideRequestTimeoutMinutes int

	// AI ranker — entirely optional. Default off. Delete these four fields
	// (plus getEnvBool) along with services/ranker/ai.go to remove the AI
	// subsystem.
	AIRankerEnabled  bool
	AIProviderURL    string
	AIProviderAPIKey string
	AIModelName      string

	// AllowPublicOSRM opts out of the prod guard that rejects the public OSRM
	// demo server. It exists so a deployment can run APP_ENV=prod (which gates
	// the JWT secret, release logging, and diagnostic endpoints) while still
	// routing against the demo server during a small tester round. It is NOT a
	// long-term setting: the demo server is rate-limited and its usage policy
	// prohibits production traffic. Self-host OSRM before any real load.
	AllowPublicOSRM bool

	// Email delivery. When both are set, the resend.com HTTP API is used.
	// When either is empty, NoopSender logs the message instead — useful in
	// dev for reading password-reset codes out of the server log.
	ResendAPIKey string
	EmailFrom    string

	// ── Matching tunables (single source of truth) ──────────────────────────
	// All distances in meters, times in seconds. Replaces the old file-level
	// consts in services/geo.go. Phase-2 fields are parsed now but only
	// consumed once bearing/road-class land.
	MatchEtaMinS        float64 // lower ETA bound: kills "about to pass / just passed"
	MatchEtaMaxS        float64 // upper ETA bound: no premature notify; re-notify on approach
	MatchMinAheadM      float64 // flat min distance ahead (replaces aheadToleranceFraction)
	MatchWalkCapM       float64 // default walk-to-pickup cap
	MatchWalkLongM      float64 // opt-in cap when passenger sets allow_long_walk
	MatchMinCarryCityM  float64 // min carried distance to be worth a stop (city)
	MatchMinCarryFastM  float64 // min carried distance on fast roads
	MatchFastRoadKmh    float64 // OSRM-implied speed at/above which a pickup is "fast road"
	MatchCarryK         float64 // carry ≥ K·(walk_in+walk_out) coupling
	MatchLocFreshS      float64 // current_location is "fresh" within this many seconds
	MatchRerunMovedM    float64 // re-run corridor when driver moved ≥ this
	MatchLiveLocS       float64 // live-location relay throttle (Phase 2)
	MatchShortTripM     float64 // trips shorter than this → "consider walking" advice
	MatchReachableWalkM float64 // off-route distance still worth a "walk closer" advice

	// Reverse trigger: when a passenger appears/updates, at most this many
	// plausible looking drivers get a targeted corridor check (Round 2 §5).
	MatchPaxTriggerMaxDrivers int

	// ── Chat (Round 2 §3) ───────────────────────────────────────────────────
	// ChatPreRideMaxPerMin caps messages/min per sender per conversation for
	// pairs with NO completed ride yet (anti-spam on pre-match chat).
	ChatPreRideMaxPerMin int
	// ChatRetentionNoRideDays: conversations between pairs that never
	// completed a ride are purged (messages cascade) after this many days of
	// inactivity. Conversations with ride history are kept indefinitely.
	ChatRetentionNoRideDays int

	// Retention periods (handlers/retention.go, Privacy Policy). Days unless
	// the name says hours.
	RetentionRideDays          int
	RetentionChatDays          int
	RetentionReportDays        int
	RetentionBugReportDays     int
	RetentionLiveLocationHours int
	RetentionPlannedTripDays   int
	// ChatCandidateRadiusKm bounds the pre-match gate: a passenger may open a
	// conversation with a driver only if that driver would appear in their
	// nearby-drivers result within this radius (mirrors the map view).
	ChatCandidateRadiusKm float64

	// ── Partial relay (Round 2 §1) ──────────────────────────────────────────
	// GTFSStaticURL: static GTFS zip for the target city (Kraków: ZTP feed).
	// Empty disables transit awareness — relay then gates on driver density.
	GTFSStaticURL string
	// GTFSStopRadiusM: a relay dropoff is "transit-connected" when a stop
	// lies within this distance of the route end.
	GTFSStopRadiusM float64
	// GTFSRefreshDays: re-download the feed when the newest imported stop is
	// older than this.
	GTFSRefreshDays int
	// MatchRelayMinDrivers: without a transit stop, a relay dropoff is only
	// feasible when at least this many OTHER looking drivers' routes pass
	// within the corridor of the dropoff (second-leg search density).
	MatchRelayMinDrivers int

	// ── Micro-detour (Round 2 §2) ───────────────────────────────────────────
	// Per-candidate caps — BOTH must hold vs a fresh current-position
	// baseline route.
	MatchMaxDetourM float64
	MatchMaxDetourS float64
	// Cumulative per-route budget across live micro_detour offers/rides.
	MatchMaxDetourTotalM float64
	MatchMaxDetourTotalS float64
	// Near-miss outer bound: candidates failing ONLY the walk cap, with
	// walk_in below this, are eligible for a detour evaluation.
	MatchDetourNearMissM float64
	// Max OSRM via-route evaluations per corridor run (cost control).
	MatchDetourMaxCandidates int

	// ── Pickup / no-show ──
	// A driver may report a no-show once a ride has been accepted for at
	// least MatchNoShowWaitS without the pickup being confirmed.
	MatchNoShowWaitS float64
	// An accepted ride never picked up is cancelled after this long, so a
	// forgotten ride doesn't hold a seat (and the passenger) forever.
	AcceptedTimeoutMinutes int

	// OSRMFootBaseURL: the walking-profile OSRM (OSRM_FOOT_BASE_URL). Empty
	// = use OSRMBaseURL (only right for servers that honour the profile).
	OSRMFootBaseURL string

	// PhotoDir holds uploaded profile photos (PHOTO_DIR). Back it up with
	// the database.
	PhotoDir string

	// ServiceAreas limits where routes can start/end and passengers can
	// search (SERVICE_AREAS, see areas.go). Empty = no limit.
	ServiceAreas []Area
}

var C Config

func Load() {
	godotenv.Load()
	C = Config{
		AppEnv:               getEnv("APP_ENV", "dev"),
		Port:                 getEnv("PORT", "8080"),
		DBHost:               getEnv("DB_HOST", "localhost"),
		DBPort:               getEnv("DB_PORT", "5432"),
		DBUser:               getEnv("DB_USER", "postgres"),
		DBPassword:           getEnv("DB_PASSWORD", "password"),
		DBName:               getEnv("DB_NAME", "rideshare"),
		DBMaxConns:           getEnvInt("DB_MAX_CONNS", 20),
		DBStatementTimeoutMs: getEnvInt("DB_STATEMENT_TIMEOUT_MS", 5000),
		JWTSecret:            getEnv("JWT_SECRET", "change-this"),
		// 168h (7 days), down from 720h (30 days): a leaked token is only as
		// dangerous as it is long-lived. Deliberate logout is handled by the
		// users.token_version bump, not by expiry.
		JWTTTLHours:               getEnvInt("JWT_TTL_HOURS", 168),
		FirebaseCredPath:          getEnv("FIREBASE_CREDENTIALS_PATH", ""),
		OSRMBaseURL:               getEnv("OSRM_BASE_URL", "http://router.project-osrm.org"),
		PhotoDir:                  getEnv("PHOTO_DIR", "./data/photos"),
		OSRMFootBaseURL:           getEnv("OSRM_FOOT_BASE_URL", ""),
		MaxCorridorNotifications:  getEnvInt("MAX_CORRIDOR_NOTIFICATIONS", 20),
		RideRequestTimeoutMinutes: getEnvInt("RIDE_REQUEST_TIMEOUT_MINUTES", 5),

		AIRankerEnabled:  getEnvBool("AI_RANKER_ENABLED", false),
		AIProviderURL:    getEnv("AI_PROVIDER_URL", ""),
		AIProviderAPIKey: getEnv("AI_PROVIDER_API_KEY", ""),
		AIModelName:      getEnv("AI_MODEL_NAME", ""),

		AllowPublicOSRM: getEnvBool("ALLOW_PUBLIC_OSRM", false),

		ResendAPIKey: getEnv("RESEND_API_KEY", ""),
		EmailFrom:    getEnv("EMAIL_FROM", ""),

		MatchEtaMinS:       getEnvFloat("MATCH_ETA_MIN_S", 90),
		MatchEtaMaxS:       getEnvFloat("MATCH_ETA_MAX_S", 600),
		MatchMinAheadM:     getEnvFloat("MATCH_MIN_AHEAD_M", 300),
		MatchWalkCapM:      getEnvFloat("MATCH_WALK_CAP_M", 600),
		MatchWalkLongM:     getEnvFloat("MATCH_WALK_LONG_M", 2000),
		MatchMinCarryCityM: getEnvFloat("MATCH_MIN_CARRY_CITY_M", 2000),
		MatchMinCarryFastM: getEnvFloat("MATCH_MIN_CARRY_FAST_M", 15000),
		MatchFastRoadKmh:   getEnvFloat("MATCH_FAST_ROAD_KMH", 80),
		MatchCarryK:        getEnvFloat("MATCH_CARRY_K", 4),
		// 120s, not 30s. Every match path gates on this, and the client can only
		// refresh it from a foreground timer — at 30s a driver whose screen
		// switched off was dropped from matching almost immediately, which reads
		// as "matching is broken" rather than as a location problem.
		MatchLocFreshS:      getEnvFloat("MATCH_LOC_FRESH_S", 120),
		MatchRerunMovedM:    getEnvFloat("MATCH_RERUN_MOVED_M", 500),
		MatchLiveLocS:       getEnvFloat("MATCH_LIVE_LOC_S", 3),
		MatchShortTripM:     getEnvFloat("MATCH_SHORT_TRIP_M", 1000),
		MatchReachableWalkM: getEnvFloat("MATCH_REACHABLE_WALK_M", 1500),

		MatchPaxTriggerMaxDrivers: getEnvInt("MATCH_PAX_TRIGGER_MAX_DRIVERS", 10),

		ChatPreRideMaxPerMin:    getEnvInt("CHAT_PRE_RIDE_MAX_PER_MIN", 5),
		ChatRetentionNoRideDays: getEnvInt("CHAT_RETENTION_NO_RIDE_DAYS", 90),

		RetentionRideDays:          getEnvInt("RETENTION_RIDE_DAYS", 3*365),
		RetentionChatDays:          getEnvInt("RETENTION_CHAT_DAYS", 365),
		RetentionReportDays:        getEnvInt("RETENTION_REPORT_DAYS", 2*365),
		RetentionBugReportDays:     getEnvInt("RETENTION_BUG_REPORT_DAYS", 365),
		RetentionLiveLocationHours: getEnvInt("RETENTION_LIVE_LOCATION_HOURS", 24),
		RetentionPlannedTripDays:   getEnvInt("RETENTION_PLANNED_TRIP_DAYS", 30),
		ChatCandidateRadiusKm:      getEnvFloat("CHAT_CANDIDATE_RADIUS_KM", 10),

		GTFSStaticURL:        getEnv("GTFS_STATIC_URL", ""),
		GTFSStopRadiusM:      getEnvFloat("GTFS_STOP_RADIUS_M", 300),
		GTFSRefreshDays:      getEnvInt("GTFS_REFRESH_DAYS", 7),
		MatchRelayMinDrivers: getEnvInt("MATCH_RELAY_MIN_DRIVERS", 2),

		MatchMaxDetourM:          getEnvFloat("MATCH_MAX_DETOUR_M", 300),
		MatchMaxDetourS:          getEnvFloat("MATCH_MAX_DETOUR_S", 90),
		MatchMaxDetourTotalM:     getEnvFloat("MATCH_MAX_DETOUR_TOTAL_M", 600),
		MatchMaxDetourTotalS:     getEnvFloat("MATCH_MAX_DETOUR_TOTAL_S", 180),
		MatchDetourNearMissM:     getEnvFloat("MATCH_DETOUR_NEAR_MISS_M", 1500),
		MatchDetourMaxCandidates: getEnvInt("MATCH_DETOUR_MAX_CANDIDATES", 3),

		MatchNoShowWaitS:       getEnvFloat("MATCH_NO_SHOW_WAIT_S", 300),
		AcceptedTimeoutMinutes: getEnvInt("ACCEPTED_TIMEOUT_MINUTES", 60),
	}
	C.ServiceAreas = loadServiceAreas(C.IsProd())
}

// IsProd reports whether the server runs in production mode.
func (c Config) IsProd() bool {
	return c.AppEnv == "prod" || c.AppEnv == "production"
}

// Validate returns an error when the configuration is unsafe to run with.
// Called from main; the server refuses to boot on failure. Dev mode only
// warns so local setups keep working with zero configuration.
func (c Config) Validate() error {
	if serviceAreasErr != nil {
		return fmt.Errorf("SERVICE_AREAS: %w", serviceAreasErr)
	}
	if !c.IsProd() {
		return nil
	}
	if c.JWTSecret == "" || c.JWTSecret == "change-this" {
		return fmt.Errorf("APP_ENV=%s requires a real JWT_SECRET (got the insecure default)", c.AppEnv)
	}
	if c.OSRMBaseURL == "http://router.project-osrm.org" && !c.AllowPublicOSRM {
		return fmt.Errorf("APP_ENV=%s requires a self-hosted OSRM_BASE_URL — the public demo server is rate-limited and prohibits production use. Set ALLOW_PUBLIC_OSRM=true to override for a short tester round", c.AppEnv)
	}
	return nil
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		switch v {
		case "1", "true", "TRUE", "True", "yes", "on":
			return true
		case "0", "false", "FALSE", "False", "no", "off":
			return false
		}
	}
	return fallback
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
