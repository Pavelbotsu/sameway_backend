package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port             string
	DBHost           string
	DBPort           string
	DBUser           string
	DBPassword       string
	DBName           string
	RedisAddr        string
	RedisPassword    string
	JWTSecret        string
	FirebaseCredPath string
	OSRMBaseURL      string
}

var C Config

func Load() {
	godotenv.Load()
	C = Config{
		Port:             getEnv("PORT", "8080"),
		DBHost:           getEnv("DB_HOST", "localhost"),
		DBPort:           getEnv("DB_PORT", "5432"),
		DBUser:           getEnv("DB_USER", "postgres"),
		DBPassword:       getEnv("DB_PASSWORD", "password"),
		DBName:           getEnv("DB_NAME", "rideshare"),
		RedisAddr:        getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:    getEnv("REDIS_PASSWORD", ""),
		JWTSecret:        getEnv("JWT_SECRET", "change-this"),
		FirebaseCredPath: getEnv("FIREBASE_CREDENTIALS_PATH", ""),
		OSRMBaseURL:      getEnv("OSRM_BASE_URL", "http://router.project-osrm.org"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
