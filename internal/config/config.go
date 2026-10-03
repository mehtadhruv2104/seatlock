package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port        string
	DatabaseURL string
	AdminKey    string
	DBMaxConns  int
	// LogSampleSeatTaken logs 1 in N reserve 409 seat_taken lines (1 = all).
	LogSampleSeatTaken int
	// Commit is the deployed git commit: Railway sets RAILWAY_GIT_COMMIT_SHA;
	// COMMIT_SHA lets other environments provide it.
	Commit string
}

func Load() (Config, error) {
	cfg := Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AdminKey:    os.Getenv("ADMIN_KEY"),
		Commit:      getEnv("RAILWAY_GIT_COMMIT_SHA", getEnv("COMMIT_SHA", "unknown")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	// Fail closed: without a key, show creation would be open to anyone.
	if cfg.AdminKey == "" {
		return Config{}, errors.New("ADMIN_KEY is required")
	}
	var err error
	if cfg.DBMaxConns, err = positiveInt("DB_MAX_CONNS", "32"); err != nil {
		return Config{}, err
	}
	if cfg.LogSampleSeatTaken, err = positiveInt("LOG_SAMPLE_SEAT_TAKEN", "100"); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func positiveInt(key, fallback string) (int, error) {
	v := getEnv(key, fallback)
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
