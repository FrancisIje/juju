// Package config loads server settings from environment variables.
//
// Environment variables (rather than a config file) are the convention for
// services that run in containers: the same binary runs everywhere and only
// the environment changes between laptop, CI and production.
package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"juju/internal/httpx"
)

type Config struct {
	Addr        string // JUJU_ADDR, address the HTTP server listens on
	DatabaseURL string // JUJU_DATABASE_URL, Postgres connection string
	Workers     int    // JUJU_WORKERS, number of worker goroutines draining the queue
	// JUJU_QUEUE_MAX_MB caps the durable queue on disk. Past it, ingest
	// answers 429 and SDKs retry later. Default 1024.
	QueueMaxMB int
	// JUJU_NATS_URL: a shared NATS JetStream server for the queue. Empty
	// runs one embedded in juju (stored under JUJU_DATA_DIR/queue), which
	// is right for a single server. Needed when ingest and workers run as
	// separate processes.
	NATSURL string
	// JUJU_RATE_LIMIT: events per second each project may send, for
	// projects without their own limit (juju project limits). 0 turns the
	// default off. Default 1000, with bursts of 5 seconds' worth.
	RateLimit int
	// JUJU_TRUSTED_PROXIES: reverse proxies whose X-Forwarded-For header is
	// believed, as addresses or CIDR ranges. Default: this machine
	// (127.0.0.1, ::1), which is Caddy in the standard deploy. Without it,
	// every visitor looks like the proxy, and per-address login limits
	// become one limit shared by everyone.
	TrustedProxies []netip.Prefix
	// JUJU_SCRUB_PII=false keeps emails, URL tokens and the like that
	// apps send by accident (see event.Scrub). Scrubbing is on by default,
	// so the field is named for turning it off: the zero value is safe.
	KeepPII bool
	// JUJU_RETENTION_DAYS: error events, app sessions and transactions
	// older than this are deleted (issues stay). Default 90; 0 keeps them.
	RetentionDays int
	// JUJU_ANALYTICS_RETENTION_DAYS: the same for analytics events in
	// ClickHouse. Default 365; 0 keeps them.
	AnalyticsRetentionDays int
	// JUJU_ROLES: which parts this process runs, comma-separated: ingest
	// (POST /api/v1/.../events), worker (stores queued events), api (the
	// dashboard and read API). Default all three.
	Roles       Roles
	LogLevel    slog.Level // JUJU_LOG_LEVEL, debug | info | warn | error
	DataDir     string     // JUJU_DATA_DIR, where uploaded debug files are kept
	MaxUploadMB int        // JUJU_MAX_UPLOAD_MB, largest debug file accepted
	// JUJU_PUBLIC_URL, how people and SDKs reach this server. Used in DSNs,
	// alert links, and to decide whether cookies need https.
	PublicURL string
	// JUJU_ALLOW_SIGNUP: anyone can create an account and their own
	// organization (on by default). Set it to false for a private server,
	// where people join only through invite links.
	AllowSignup bool
	// JUJU_ALLOW_HTTP_WEBHOOKS lets alerts post to http:// URLs. Only for
	// local testing: see alert.ValidateWebhookURL.
	AllowHTTPWebhooks bool
	SMTPAddr          string // JUJU_SMTP_ADDR, host:port; empty turns email alerts off
	SMTPUser          string // JUJU_SMTP_USER
	SMTPPassword      string // JUJU_SMTP_PASSWORD
	SMTPFrom          string // JUJU_SMTP_FROM
	// JUJU_CLICKHOUSE_URL (e.g. http://127.0.0.1:8123) turns on product
	// analytics. Empty: error tracking works as before, analytics is off.
	ClickHouseURL string
	// JUJU_ANALYTICS_QUEUE_MAX_MB caps the analytics events waiting on disk
	// for ClickHouse. Past it the oldest are dropped; ingest keeps answering.
	AnalyticsQueueMaxMB int
	ClickHouseUser      string // JUJU_CLICKHOUSE_USER
	ClickHousePassword  string // JUJU_CLICKHOUSE_PASSWORD
	ClickHouseDatabase  string // JUJU_CLICKHOUSE_DATABASE, default juju
	// JUJU_MONITOR_DSN: a project on a second juju instance that this server
	// reports its own errors and health to. Empty turns that off.
	MonitorDSN string
}

const DefaultDatabaseURL = "postgres://juju:juju@localhost:5432/juju?sslmode=disable"

// Load reads the environment, falling back to defaults that work with
// `make devdb` / docker compose.
func Load() (Config, error) {
	c := Config{
		Addr:               getenv("JUJU_ADDR", ":8080"),
		DatabaseURL:        getenv("JUJU_DATABASE_URL", DefaultDatabaseURL),
		DataDir:            getenv("JUJU_DATA_DIR", "data"),
		PublicURL:          strings.TrimRight(getenv("JUJU_PUBLIC_URL", "http://localhost:8080"), "/"),
		AllowSignup:        !isOff(getenv("JUJU_ALLOW_SIGNUP", "true")),
		AllowHTTPWebhooks:  getenv("JUJU_ALLOW_HTTP_WEBHOOKS", "") == "true",
		SMTPAddr:           getenv("JUJU_SMTP_ADDR", ""),
		SMTPUser:           getenv("JUJU_SMTP_USER", ""),
		SMTPPassword:       getenv("JUJU_SMTP_PASSWORD", ""),
		SMTPFrom:           getenv("JUJU_SMTP_FROM", "juju <alerts@localhost>"),
		ClickHouseURL:      getenv("JUJU_CLICKHOUSE_URL", ""),
		ClickHouseUser:     getenv("JUJU_CLICKHOUSE_USER", ""),
		ClickHousePassword: getenv("JUJU_CLICKHOUSE_PASSWORD", ""),
		ClickHouseDatabase: getenv("JUJU_CLICKHOUSE_DATABASE", "juju"),
		MonitorDSN:         getenv("JUJU_MONITOR_DSN", ""),
	}
	var err error
	if c.QueueMaxMB, err = getint("JUJU_QUEUE_MAX_MB", 1024); err != nil {
		return c, err
	}
	if c.AnalyticsQueueMaxMB, err = getint("JUJU_ANALYTICS_QUEUE_MAX_MB", 512); err != nil {
		return c, err
	}
	if c.AnalyticsQueueMaxMB < 1 {
		return c, fmt.Errorf("JUJU_ANALYTICS_QUEUE_MAX_MB must be at least 1")
	}
	c.NATSURL = getenv("JUJU_NATS_URL", "")
	c.KeepPII = isOff(getenv("JUJU_SCRUB_PII", "true"))
	if c.RateLimit, err = getint("JUJU_RATE_LIMIT", 1000); err != nil {
		return c, err
	}
	if c.TrustedProxies, err = httpx.ParsePrefixes(getenv("JUJU_TRUSTED_PROXIES", "127.0.0.1,::1")); err != nil {
		return c, fmt.Errorf("JUJU_TRUSTED_PROXIES: %w", err)
	}
	if c.RetentionDays, err = getint("JUJU_RETENTION_DAYS", 90); err != nil {
		return c, err
	}
	if c.AnalyticsRetentionDays, err = getint("JUJU_ANALYTICS_RETENTION_DAYS", 365); err != nil {
		return c, err
	}
	if c.Roles, err = ParseRoles(getenv("JUJU_ROLES", "ingest,worker,api")); err != nil {
		return c, err
	}
	// The embedded queue lives inside one process: ingest there and workers
	// elsewhere would never meet. Refuse to start rather than queue events
	// nobody will store.
	if c.NATSURL == "" && c.Roles.Ingest != c.Roles.Worker {
		return c, fmt.Errorf("JUJU_ROLES=%s runs ingest and workers in separate processes, so they need a shared queue: set JUJU_NATS_URL", c.Roles)
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
	if c.QueueMaxMB < 1 || c.Workers < 1 {
		return c, fmt.Errorf("JUJU_QUEUE_MAX_MB and JUJU_WORKERS must be at least 1")
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

// isOff reads a switch that's on by default: only a clear "off" turns it
// off, so a typo can't close something by accident.
func isOff(v string) bool {
	switch strings.ToLower(v) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

// Roles is which parts of juju a process runs.
type Roles struct{ Ingest, Worker, API bool }

func ParseRoles(s string) (Roles, error) {
	var r Roles
	for _, part := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "ingest":
			r.Ingest = true
		case "worker":
			r.Worker = true
		case "api":
			r.API = true
		case "all":
			r = Roles{true, true, true}
		case "":
		default:
			return r, fmt.Errorf("JUJU_ROLES: unknown role %q (use ingest, worker, api)", part)
		}
	}
	if r == (Roles{}) {
		return r, fmt.Errorf("JUJU_ROLES: choose at least one of ingest, worker, api")
	}
	return r, nil
}

func (r Roles) String() string {
	var parts []string
	if r.Ingest {
		parts = append(parts, "ingest")
	}
	if r.Worker {
		parts = append(parts, "worker")
	}
	if r.API {
		parts = append(parts, "api")
	}
	return strings.Join(parts, ",")
}
