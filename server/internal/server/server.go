// Package server wires the pieces together and runs them until shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"juju/internal/alert"
	"juju/internal/analytics"
	"juju/internal/api"
	"juju/internal/auth"
	"juju/internal/blob"
	"juju/internal/config"
	"juju/internal/httpx"
	"juju/internal/ingest"
	"juju/internal/live"
	"juju/internal/queue"
	"juju/internal/store"
	"juju/internal/symbolicate"
	"juju/internal/ui"
	"juju/internal/web"
	"juju/internal/worker"
)

// Routes builds the HTTP handler. Kept separate from Run so tests can mount
// it on an httptest.Server.
// Deps is everything the HTTP routes use.
type Deps struct {
	Pool      *pgxpool.Pool
	Store     *store.Store
	Queue     queue.Publisher
	Blobs     *blob.Store
	MaxUpload int64
	Auth      *auth.Service
	Alerts    *alert.Notifier
	Live      *live.Hub
	// Changes announces issue changes to every API process (the live Bus).
	// Nil: Live directly, for a single process.
	Changes   interface{ Changed(projectID int64) }
	Analytics *analytics.Client // nil: analytics off
	// QueueReady reports whether the queue can be reached, for /readyz.
	QueueReady func() bool
	Limits     *ingest.Limits // nil: no rate limits or quotas
	Config     config.Config
	Log        *slog.Logger
}

func Routes(d Deps) http.Handler {
	pool, st, q, blobs, maxUpload, log := d.Pool, d.Store, d.Queue, d.Blobs, d.MaxUpload, d.Log
	roles := d.Config.Roles
	if roles == (config.Roles{}) {
		roles = config.Roles{Ingest: true, Worker: true, API: true} // tests: everything, as before roles existed
	}
	mux := http.NewServeMux()

	// Liveness: "is the process up?" Never touches the database, so a
	// database outage doesn't make an orchestrator restart healthy servers.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	// Readiness: "can it do useful work right now?"
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if pool != nil {
			if err := pool.Ping(ctx); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if d.QueueReady != nil && !d.QueueReady() {
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready\n"))
	})

	if roles.Ingest {
		mux.HandleFunc("OPTIONS /api/v1/projects/{id}/events", ingest.Preflight)
		mux.Handle("POST /api/v1/projects/{id}/events", &ingest.Handler{
			Projects: &ingest.ProjectCache{Src: st, TTL: 30 * time.Second, Now: time.Now},
			Limits:   d.Limits, KeepPII: d.Config.KeepPII, Queue: q, Log: log, Now: time.Now,
		})
	}
	if roles.API {
		// The same "an issue changed" signal the dashboard sends, so a status
		// change from a script or agent shows up in open dashboards too.
		var changes interface{ Changed(projectID int64) } = d.Changes
		if changes == nil && d.Live != nil {
			changes = d.Live
		}
		(&api.API{Store: st, Blobs: blobs, MaxUpload: maxUpload, PublicURL: d.Config.PublicURL, Changes: changes, Log: log}).Register(mux)
		(&ui.API{Store: st, Auth: d.Auth, Alerts: d.Alerts, Live: d.Live, Changes: d.Changes, Analytics: d.Analytics, PublicURL: d.Config.PublicURL, AllowSignup: d.Config.AllowSignup, TrustedProxies: d.Config.TrustedProxies, Log: log}).Register(mux)
	}
	// An unknown /api/ path must be a JSON 404, not the app's HTML. On an
	// ingest-only process that's every API but ingest.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "no such API route")
	})
	if roles.API {
		mux.Handle("/", web.Handler()) // everything else is the dashboard
	}
	return securityHeaders(mux, strings.HasPrefix(d.Config.PublicURL, "https://"))
}

// securityHeaders adds headers every response should have, API or page.
//
//   - nosniff: browsers use the declared Content-Type instead of guessing.
//     Event data is the attacker's to write, so a JSON response a browser
//     "sniffed" as HTML could run script.
//   - HSTS (on https only): after one visit the browser uses https for this
//     host for a year, even for a typed http:// link, so nobody on the
//     network can downgrade a later visit and read the session cookie.
func securityHeaders(next http.Handler, https bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if https {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// Run serves HTTP and runs workers until ctx is cancelled (Ctrl-C/SIGTERM),
// running the parts cfg.Roles asks for.
//
// Events go through a durable queue (JetStream): ingest answers 202 only
// after an event is stored there, and a worker acknowledges it only after
// it's in Postgres. So a crash, kill -9 or restart at any point loses
// nothing: unacknowledged events are delivered again. Shutdown is still
// orderly, to avoid needless redelivery:
//
//  1. stop accepting HTTP requests and finish in-flight ones
//  2. stop taking events from the queue; hand back any not yet started
//  3. let workers finish (and acknowledge) the events in their hands
//  4. flush alerts and analytics, then close the queue
func Run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) error {
	roles := cfg.Roles
	if roles == (config.Roles{}) {
		roles = config.Roles{Ingest: true, Worker: true, API: true}
	}
	st := store.New(pool)
	blobs, err := blob.New(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return err
	}

	var q *queue.JetStream
	if roles.Ingest || roles.Worker || cfg.NATSURL != "" {
		q, err = queue.OpenJetStream(ctx, queue.JetStreamConfig{
			URL: cfg.NATSURL, Dir: filepath.Join(cfg.DataDir, "queue"), MaxBytes: int64(cfg.QueueMaxMB) << 20,
			Fetch: max(16, 4*cfg.Workers), Log: log,
			Analytics: cfg.ClickHouseURL != "", AnalyticsMaxBytes: int64(cfg.AnalyticsQueueMaxMB) << 20,
		})
		if err != nil {
			return fmt.Errorf("queue: %w", err)
		}
		where := cfg.NATSURL
		if where == "" {
			where = "embedded, in " + filepath.Join(cfg.DataDir, "queue")
		}
		pending, _ := q.Pending(ctx)
		log.Info("queue ready", "nats", where, "waiting", pending)
	}
	defer func() {
		if q != nil {
			q.Close()
		}
	}()

	hub := live.NewHub()
	var bus *live.Bus
	var changes interface{ Changed(int64) } = hub
	if q != nil {
		bus = &live.Bus{NC: q.Conn(), Log: log}
		changes = bus
		if roles.API {
			if _, err := bus.Deliver(hub); err != nil {
				return fmt.Errorf("live updates: %w", err)
			}
		}
	}

	var alerts *alert.Notifier
	var ch *analytics.Client
	var chWriter *analytics.Writer
	if roles.API || roles.Worker {
		alerts = &alert.Notifier{
			Settings:          st,
			SMTP:              alert.SMTPConfig{Addr: cfg.SMTPAddr, Username: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom},
			PublicURL:         cfg.PublicURL,
			AllowHTTPWebhooks: cfg.AllowHTTPWebhooks,
			Log:               log,
		}
		alerts.Start(2)
		ch, chWriter = startAnalytics(ctx, cfg, roles, q, log)
	}

	if roles.Worker {
		go runRetention(ctx, st, cfg.RetentionDays, log)
	}

	workersDone := make(chan struct{})
	if roles.Worker {
		sym := &symbolicate.Service{Files: symbolicate.StoredFiles{Store: st, Blobs: blobs}, Log: log}
		var analyticsSink worker.Analytics
		if chWriter != nil {
			analyticsSink = q
		}
		go func() {
			// Workers get a fresh context, not ctx: they must finish the
			// event in hand after ctx is cancelled.
			worker.Run(context.Background(), q, worker.Deps{Store: st, Symbolicator: sym, Alerter: alerts, Analytics: analyticsSink, Live: changes, Log: log}, cfg.Workers)
			close(workersDone)
		}()
	} else {
		close(workersDone)
	}

	// Rate limits and quotas live in ingest. Usage counts are written to
	// Postgres every few seconds, and once more at shutdown.
	var limits *ingest.Limits
	limitsDone := make(chan struct{})
	limitsCtx, stopLimits := context.WithCancel(context.Background())
	defer stopLimits()
	if roles.Ingest {
		limits = &ingest.Limits{DefaultRate: cfg.RateLimit, Usage: st, Now: time.Now, Log: log}
		go func() {
			limits.Run(limitsCtx, 5*time.Second)
			close(limitsDone)
		}()
	} else {
		close(limitsDone)
	}

	var publisher queue.Publisher
	var queueReady func() bool
	if q != nil {
		publisher = q
		queueReady = func() bool { return q.Conn().IsConnected() }
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           Routes(Deps{Pool: pool, Store: st, Queue: publisher, QueueReady: queueReady, Blobs: blobs, MaxUpload: int64(cfg.MaxUploadMB) << 20, Auth: &auth.Service{Store: st, SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://")}, Alerts: alerts, Live: hub, Changes: changes, Analytics: ch, Limits: limits, Config: cfg, Log: log}),
		ReadHeaderTimeout: 5 * time.Second,  // slow clients can't hold connections open forever
		ReadTimeout:       30 * time.Second, // a 1 MB body on a bad mobile network
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Live streams never end by themselves; close them first, or Shutdown
	// would wait its full timeout for them.
	srv.RegisterOnShutdown(hub.Close)
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	log.Info("juju listening", "addr", ln.Addr().String(), "roles", roles.String(), "workers", cfg.Workers)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var runErr error
	select {
	case runErr = <-serveErr:
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http shutdown", "err", err)
		}
	}
	stopLimits() // no more requests: write the last usage counts
	<-limitsDone
	if roles.Worker {
		q.StopDeliveries()
	}
	<-workersDone
	if alerts != nil {
		alerts.Stop() // after the workers: they may have just queued alerts
	}
	if chWriter != nil {
		// After the workers too: they may have just moved old events across.
		// Whatever this doesn't write stays in the queue for next time.
		q.StopAnalytics()
		chWriter.Wait()
		if left, err := q.AnalyticsPending(context.Background()); err == nil {
			log.Info("analytics stopped", "events_waiting_in_queue", left)
		}
	}
	if q != nil {
		left, _ := q.Pending(context.Background())
		log.Info("shutdown complete", "events_waiting_in_queue", left)
	}
	return runErr
}

// runRetention deletes expired data every hour until ctx is done. It runs
// on every worker process; a Postgres advisory lock makes sure only one of
// them works at a time (see store.DeleteExpired). The first run waits a
// minute, so a deploy's restart isn't also a burst of deletes.
func runRetention(ctx context.Context, st *store.Store, days int, log *slog.Logger) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = time.Hour
		now := time.Now()
		var before time.Time // zero time: nothing is older, so data is kept
		if days > 0 {
			before = now.AddDate(0, 0, -days)
		}
		start := time.Now()
		d, ran, err := st.DeleteExpired(ctx, before, now, 5000)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("retention", "err", err, "deleted", d)
			}
			continue
		}
		if ran {
			log.Info("retention", "older_than_days", days, "deleted", d, "took", time.Since(start).Round(time.Millisecond).String())
		}
	}
}

// startAnalytics connects to ClickHouse if configured. juju starts either
// way: errors are the core job, so a ClickHouse that's down or slow to start
// mustn't stop them. Tables are created in the background, retrying, and the
// writer (on workers) holds events until it can write them.
func startAnalytics(ctx context.Context, cfg config.Config, roles config.Roles, q *queue.JetStream, log *slog.Logger) (*analytics.Client, *analytics.Writer) {
	if cfg.ClickHouseURL == "" {
		log.Info("analytics off: set JUJU_CLICKHOUSE_URL to turn it on")
		return nil, nil
	}
	ch := &analytics.Client{URL: cfg.ClickHouseURL, User: cfg.ClickHouseUser, Password: cfg.ClickHousePassword, Database: cfg.ClickHouseDatabase, RetentionDays: cfg.AnalyticsRetentionDays}
	var w *analytics.Writer
	if roles.Worker {
		w = &analytics.Writer{Client: ch, Log: log}
		w.Start(q.Analytics())
	}
	go func() {
		for attempt := 1; ; attempt++ {
			err := ch.Migrate(ctx)
			if err == nil {
				log.Info("analytics on", "clickhouse", cfg.ClickHouseURL)
				return
			}
			if ctx.Err() != nil {
				return
			}
			if attempt == 1 || attempt%12 == 0 {
				log.Error("analytics: can't set up ClickHouse yet, retrying every 5s", "err", err)
			}
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, w
}
