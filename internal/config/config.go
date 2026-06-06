package config

import (
	"fmt"
	"os"
)

type Config struct {
	ABSUrl          string
	ABSToken        string
	HardcoverToken  string
	CronSchedule    string
	CronTimezone    string
	DBPath          string
	Port            string
	SMTPHost        string
	SMTPPort        string
	SMTPUser        string
	SMTPPass        string
	SMTPFrom        string
	SMTPTo          string
}

// SMTPEnabled reports whether SMTP is configured (host and recipient are required).
func (c *Config) SMTPEnabled() bool {
	return c.SMTPHost != "" && c.SMTPTo != ""
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
		SMTPHost:       os.Getenv("SMTP_HOST"),
		SMTPPort:       getEnvOr("SMTP_PORT", "587"),
		SMTPUser:       os.Getenv("SMTP_USER"),
		SMTPPass:       os.Getenv("SMTP_PASS"),
		SMTPFrom:       os.Getenv("SMTP_FROM"),
		SMTPTo:         os.Getenv("SMTP_TO"),
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
