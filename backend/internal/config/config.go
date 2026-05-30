package config

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port              string
	PublicURL         string
	DatabaseURL       string
	JWTSecret         string
	MaxUploadBytes    int64
	RequestTimeout    time.Duration
	GoogleDriveAPIKey string
	MinIOEndpoint     string
	MinIOAccessKey    string
	MinIOSecretKey    string
	MinIOBucket       string
	MinIOUseSSL       bool
	AllowedOrigin     string
	RedisAddr         string
	RedisPassword     string
	RedisDB           int
}

// Load centralizes runtime configuration so local dev and production boot the same way.
func Load() (Config, error) {
	port := envOrDefault("BACKEND_PORT", "8080")
	publicURL := envOrDefault("BACKEND_PUBLIC_URL", "http://localhost:"+port)
	maxUploadBytes, err := loadMaxUploadBytes()
	if err != nil {
		return Config{}, err
	}

	timeoutSeconds, err := strconv.Atoi(envOrDefault("REQUEST_TIMEOUT_SECONDS", "60"))
	if err != nil {
		return Config{}, fmt.Errorf("parse REQUEST_TIMEOUT_SECONDS: %w", err)
	}

	minioSSL, err := strconv.ParseBool(envOrDefault("MINIO_USE_SSL", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("parse MINIO_USE_SSL: %w", err)
	}

	redisDB, err := strconv.Atoi(envOrDefault("REDIS_DB", "0"))
	if err != nil {
		return Config{}, fmt.Errorf("parse REDIS_DB: %w", err)
	}

	cfg := Config{
		Port:              port,
		PublicURL:         publicURL,
		DatabaseURL:       envOrDefault("DATABASE_URL", "postgres://mergepdf:mergepdf@localhost:5432/mergepdf?sslmode=disable"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		MaxUploadBytes:    maxUploadBytes,
		RequestTimeout:    time.Duration(timeoutSeconds) * time.Second,
		GoogleDriveAPIKey: os.Getenv("GOOGLE_DRIVE_API_KEY"),
		MinIOEndpoint:     envOrDefault("MINIO_ENDPOINT", "localhost:9000"),
		MinIOAccessKey:    envOrDefault("MINIO_ACCESS_KEY", "minioadmin"),
		MinIOSecretKey:    envOrDefault("MINIO_SECRET_KEY", "minioadmin"),
		MinIOBucket:       envOrDefault("MINIO_BUCKET", "merged-pdfs"),
		MinIOUseSSL:       minioSSL,
		AllowedOrigin:     envOrDefault("ALLOWED_ORIGIN", "http://localhost:5173"),
		RedisAddr:         os.Getenv("REDIS_ADDR"),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		RedisDB:           redisDB,
	}

	if cfg.JWTSecret == "" {
		return Config{}, fmt.Errorf("JWT_SECRET is required")
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func loadMaxUploadBytes() (int64, error) {
	if raw := os.Getenv("MAX_UPLOAD_SIZE"); raw != "" {
		size, err := parseByteSize(raw)
		if err != nil {
			return 0, fmt.Errorf("parse MAX_UPLOAD_SIZE: %w", err)
		}
		return size, nil
	}

	maxUploadMB, err := strconv.Atoi(envOrDefault("MAX_UPLOAD_MB", "25"))
	if err != nil {
		return 0, fmt.Errorf("parse MAX_UPLOAD_MB: %w", err)
	}
	return int64(maxUploadMB) * 1024 * 1024, nil
}

var byteSizePattern = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgt]?b?|[kmgt])?\s*$`)

func parseByteSize(raw string) (int64, error) {
	matches := byteSizePattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(raw)))
	if len(matches) != 3 {
		return 0, fmt.Errorf("invalid size %q", raw)
	}

	value, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("parse numeric size: %w", err)
	}

	multiplier := float64(1)
	switch matches[2] {
	case "", "b":
		multiplier = 1
	case "k", "kb":
		multiplier = 1024
	case "m", "mb":
		multiplier = 1024 * 1024
	case "g", "gb":
		multiplier = 1024 * 1024 * 1024
	case "t", "tb":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unsupported unit %q", matches[2])
	}

	size := value * multiplier
	if size <= 0 {
		return 0, fmt.Errorf("size must be positive")
	}
	if size > math.MaxInt64 {
		return 0, fmt.Errorf("size too large")
	}
	return int64(size), nil
}
