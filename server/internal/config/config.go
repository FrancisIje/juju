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
	DataDir     string     // JUJU_DATA_DIR, where uploaded debug files are kept
	MaxUploadMB int        // JUJU_MAX_UPLOAD_MB, largest debug file accepted
	// JUJU_PUBLIC_URL, how people and SDKs reach this server. Used in DSNs,
	// alert links, and to decide whether cookies need https.
	PublicURL   string
	AllowSignup bool // JUJU_ALLOW_SIGNUP, let anyone create an account (the first account is always allowed)
	// JUJU_ALLOW_HTTP_WEBHOOKS lets alerts post to http:// URLs. Only for
	// local testing: see alert.ValidateWebhookURL.
	AllowHTTPWebhooks bool
	SMTPAddr          string // JUJU_SMTP_ADDR, host:port; empty turns email alerts off
	SMTPUser          string // JUJU_SMTP_USER
	SMTPPassword      string // JUJU_SMTP_PASSWORD
	SMTPFrom          string // JUJU_SMTP_FROM
}

const DefaultDatabaseURL = "postgres://juju:juju@localhost:5432/juju?sslmode=disable"

// Load reads the environment, falling back to defaults that work with
// `make devdb` / docker compose.
func Load() (Config, error) {
	c := Config{
		Addr:              getenv("JUJU_ADDR", ":8080"),
		DatabaseURL:       getenv("JUJU_DATABASE_URL", DefaultDatabaseURL),
		DataDir:           getenv("JUJU_DATA_DIR", "data"),
		PublicURL:         strings.TrimRight(getenv("JUJU_PUBLIC_URL", "http://localhost:8080"), "/"),
		AllowSignup:       getenv("JUJU_ALLOW_SIGNUP", "") == "true",
		AllowHTTPWebhooks: getenv("JUJU_ALLOW_HTTP_WEBHOOKS", "") == "true",
		SMTPAddr:          getenv("JUJU_SMTP_ADDR", ""),
		SMTPUser:          getenv("JUJU_SMTP_USER", ""),
		SMTPPassword:      getenv("JUJU_SMTP_PASSWORD", ""),
		SMTPFrom:          getenv("JUJU_SMTP_FROM", "juju <alerts@localhost>"),
	}
	var err error
	if c.QueueSize, err = getint("JUJU_QUEUE_SIZE", 10_000); err != nil {
		return c, err
	}
	if c.Workers, err = getint("JUJU_WORKERS", 2); err != nil {
		return c, err
	}
	// Flutter symbol files for a large app can be well over 100 MB.
	if c.MaxUploadMB, err = getint("JUJU_MAX_UPLOAD_MB", 512); err != nil {
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
