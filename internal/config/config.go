package config

import (
	"errors"
	"os"
)

type Config struct {
	Port        string
	DatabaseURL string
	AdminKey    string
}

func Load() (Config, error) {
	cfg := Config{
		Port:        getEnv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AdminKey:    os.Getenv("ADMIN_KEY"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	// Fail closed: without a key, show creation would be open to anyone.
	if cfg.AdminKey == "" {
		return Config{}, errors.New("ADMIN_KEY is required")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
