package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hospital/internal/surgeon"
)

func main() {

	// implementing log level
	logLevel := slog.LevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch v {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			logLevel = slog.LevelInfo
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	if err := run(log); err != nil && err != context.Canceled {
		log.Error("surgeon stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	id := os.Getenv("SURGEON_ID")
	base := os.Getenv("HOSPITAL_URL")
	if id == "" || base == "" {
		return fmt.Errorf("SURGEON_ID and HOSPITAL_URL are required")
	}
	pollTimeout := 25 * time.Second
	if v := os.Getenv("POLL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("POLL_TIMEOUT: %w", err)
		}
		pollTimeout = d
	}
	interval := 2 * time.Second
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("POLL_INTERVAL: %w", err)
		}
		interval = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("surgeon polling", "id", id, "hospital", base)
	agent := &surgeon.Agent{
		BaseURL:  base,
		ID:       id,
		Token:    os.Getenv("HOSPITAL_TOKEN"),
		Interval: interval,
		Client:   &http.Client{Timeout: pollTimeout + 10*time.Second},
		Log:      log,
	}
	return agent.Run(ctx)
}
