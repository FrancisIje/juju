// Package config loads server settings from environment variables.
//
// Environment variables (rather than a config file) are the convention for
// services that run in containers: the same binary runs everywhere and only
// the environment changes between laptop, CI and production.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr        string     // JUJU_ADDR, address the HTTP server listens on
	DatabaseURL string     // JUJU_DATABASE_URL, Postgres connection string
	QueueSize   int        // JUJU_QUEUE_SIZE, buffered channel capacity between ingest and workers
	Workers     int        // JUJU_WORKERS, number of worker goroutines draining the queue
	LogLevel    slog.Level // JUJU_LOG_LEVEL, debug | info | warn | error
}

const DefaultDatabaseURL = "postgres://juju:juju@localhost:5432/juju?sslmode=disable"

// Load reads the environment, falling back to defaults that work with
// `make devdb` / docker compose.
func Load() (Config, error) {
	c := Config{
		Addr:        getenv("JUJU_ADDR", ":8080"),
		DatabaseURL: getenv("JUJU_DATABASE_URL", DefaultDatabaseURL),
	}
	var err error
	if c.QueueSize, err = getint("JUJU_QUEUE_SIZE", 10_000); err != nil {
		return c, err
	}
	if c.Workers, err = getint("JUJU_WORKERS", 2); err != nil {
		return c, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(getenv("JUJU_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("JUJU_LOG_LEVEL: %w", err)
	}
	if c.QueueSize < 1 || c.Workers < 1 {
		return c, fmt.Errorf("JUJU_QUEUE_SIZE and JUJU_WORKERS must be at least 1")
	}
	return c, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getint(key string, fallback int) (int, error) {
	v := getenv(key, "")
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", key, v)
	}
	return n, nil
}
