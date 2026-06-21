package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                      string
	DBHost                    string
	DBPort                    string
	DBUser                    string
	DBPassword                string
	DBName                    string
	RedisAddr                 string
	RedisPassword             string
	JWTSecret                 string
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

	// Email delivery. When both are set, the resend.com HTTP API is used.
	// When either is empty, NoopSender logs the message instead — useful in
	// dev for reading password-reset codes out of the server log.
	ResendAPIKey string
	EmailFrom    string

	// ── Matching tunables (single source of truth) ──────────────────────────
	// All distances in meters, times in seconds. Replaces the old file-level
	// consts in services/geo.go. Phase-2 fields are parsed now but only
	// consumed once bearing/road-class land.
	MatchEtaMinS       float64 // lower ETA bound: kills "about to pass / just passed"
	MatchEtaMaxS       float64 // upper ETA bound: no premature notify; re-notify on approach
	MatchMinAheadM     float64 // flat min distance ahead (replaces aheadToleranceFraction)
	MatchWalkCapM      float64 // default walk-to-pickup cap
	MatchWalkLongM     float64 // opt-in cap when passenger sets allow_long_walk
	MatchMinCarryCityM float64 // min carried distance to be worth a stop (city)
	MatchMinCarryFastM float64 // min carried distance on fast roads (Phase 2)
	MatchCarryK        float64 // carry ≥ K·(walk_in+walk_out) coupling
	MatchMaxBearingDeg float64 // heading reject threshold (Phase 2)
	MatchLocFreshS     float64 // current_location is "fresh" within this many seconds
	MatchRerunMovedM   float64 // re-run corridor when driver moved ≥ this
	MatchReapBufferS   float64 // reaper ETA buffer (Phase 2 dynamic TTL)
	MatchLiveLocS      float64 // live-location relay throttle (Phase 2)
}

var C Config

func Load() {
	godotenv.Load()
	C = Config{
		Port:                      getEnv("PORT", "8080"),
		DBHost:                    getEnv("DB_HOST", "localhost"),
		DBPort:                    getEnv("DB_PORT", "5432"),
		DBUser:                    getEnv("DB_USER", "postgres"),
		DBPassword:                getEnv("DB_PASSWORD", "password"),
		DBName:                    getEnv("DB_NAME", "rideshare"),
		RedisAddr:                 getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:             getEnv("REDIS_PASSWORD", ""),
		JWTSecret:                 getEnv("JWT_SECRET", "change-this"),
		FirebaseCredPath:          getEnv("FIREBASE_CREDENTIALS_PATH", ""),
		OSRMBaseURL:               getEnv("OSRM_BASE_URL", "http://router.project-osrm.org"),
		MaxCorridorNotifications:  getEnvInt("MAX_CORRIDOR_NOTIFICATIONS", 20),
		RideRequestTimeoutMinutes: getEnvInt("RIDE_REQUEST_TIMEOUT_MINUTES", 5),

		AIRankerEnabled:  getEnvBool("AI_RANKER_ENABLED", false),
		AIProviderURL:    getEnv("AI_PROVIDER_URL", ""),
		AIProviderAPIKey: getEnv("AI_PROVIDER_API_KEY", ""),
		AIModelName:      getEnv("AI_MODEL_NAME", ""),

		ResendAPIKey: getEnv("RESEND_API_KEY", ""),
		EmailFrom:    getEnv("EMAIL_FROM", ""),

		MatchEtaMinS:       getEnvFloat("MATCH_ETA_MIN_S", 90),
		MatchEtaMaxS:       getEnvFloat("MATCH_ETA_MAX_S", 600),
		MatchMinAheadM:     getEnvFloat("MATCH_MIN_AHEAD_M", 300),
		MatchWalkCapM:      getEnvFloat("MATCH_WALK_CAP_M", 600),
		MatchWalkLongM:     getEnvFloat("MATCH_WALK_LONG_M", 2000),
		MatchMinCarryCityM: getEnvFloat("MATCH_MIN_CARRY_CITY_M", 2000),
		MatchMinCarryFastM: getEnvFloat("MATCH_MIN_CARRY_FAST_M", 15000),
		MatchCarryK:        getEnvFloat("MATCH_CARRY_K", 4),
		MatchMaxBearingDeg: getEnvFloat("MATCH_MAX_BEARING_DEG", 60),
		MatchLocFreshS:     getEnvFloat("MATCH_LOC_FRESH_S", 30),
		MatchRerunMovedM:   getEnvFloat("MATCH_RERUN_MOVED_M", 500),
		MatchReapBufferS:   getEnvFloat("MATCH_REAP_BUFFER_S", 60),
		MatchLiveLocS:      getEnvFloat("MATCH_LIVE_LOC_S", 3),
	}
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
