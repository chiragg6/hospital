package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the process configuration, read from the environment.
type Config struct {
	Addr                  string
	DatabaseURL           string
	RunbooksFile          string
	Token                 string
	SlackWebhook          string
	AllowedNamespaces     []string
	AllowSystemNamespaces bool
	DryRun                bool
	WorkerInterval        time.Duration
	PollTimeout           time.Duration
	PollInterval          time.Duration
	StaleAfter            time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Addr:                  env("HOSPITAL_ADDR", ":8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		RunbooksFile:          os.Getenv("RUNBOOKS_FILE"),
		Token:                 os.Getenv("HOSPITAL_TOKEN"),
		SlackWebhook:          os.Getenv("SLACK_WEBHOOK_URL"),
		AllowedNamespaces:     splitCSV(os.Getenv("ALLOWED_NAMESPACES")),
		AllowSystemNamespaces: envBool("ALLOW_SYSTEM_NAMESPACES"),
		DryRun:                envBool("DRY_RUN"),
		WorkerInterval:        2 * time.Second,
		PollTimeout:           25 * time.Second,
		PollInterval:          time.Second,
		StaleAfter:            15 * time.Minute,
	}
	var err error
	if cfg.WorkerInterval, err = envDuration("WORKER_INTERVAL", cfg.WorkerInterval); err != nil {
		return Config{}, err
	}
	if cfg.PollTimeout, err = envDuration("POLL_TIMEOUT", cfg.PollTimeout); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval, err = envDuration("POLL_INTERVAL", cfg.PollInterval); err != nil {
		return Config{}, err
	}
	if cfg.StaleAfter, err = envDuration("STALE_AFTER", cfg.StaleAfter); err != nil {
		return Config{}, err
	}
	if cfg.PollInterval <= 0 || cfg.PollTimeout <= 0 || cfg.WorkerInterval <= 0 || cfg.StaleAfter <= 0 {
		return Config{}, fmt.Errorf("intervals must be positive")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
