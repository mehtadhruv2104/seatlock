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
	maxConns := getEnv("DB_MAX_CONNS", "32")
	n, err := strconv.Atoi(maxConns)
	if err != nil || n < 1 {
		return Config{}, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", maxConns)
	}
	cfg.DBMaxConns = n
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
