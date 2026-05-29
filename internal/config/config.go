package config

import (
	"fmt"
	"os"
)

type Config struct {
	ABSUrl         string
	ABSToken       string
	HardcoverToken string
	CronSchedule   string
	CronTimezone   string
	DBPath         string
	Port           string
}

func Load() (*Config, error) {
	c := &Config{
		ABSUrl:         os.Getenv("ABS_URL"),
		ABSToken:       os.Getenv("ABS_TOKEN"),
		HardcoverToken: os.Getenv("HARDCOVER_TOKEN"),
		CronSchedule:   getEnvOr("CRON_SCHEDULE", "0 3 * * *"),
		CronTimezone:   getEnvOr("CRON_TIMEZONE", "Europe/Prague"),
		DBPath:         getEnvOr("DB_PATH", "./app.db"),
		Port:           getEnvOr("PORT", "8080"),
	}

	if c.ABSUrl == "" {
		return nil, fmt.Errorf("ABS_URL is required")
	}
	if c.ABSToken == "" {
		return nil, fmt.Errorf("ABS_TOKEN is required")
	}
	if c.HardcoverToken == "" {
		return nil, fmt.Errorf("HARDCOVER_TOKEN is required")
	}

	return c, nil
}

func getEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
