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
	}
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
