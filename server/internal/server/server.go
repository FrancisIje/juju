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
	"juju/internal/api"
	"juju/internal/auth"
	"juju/internal/blob"
	"juju/internal/config"
	"juju/internal/httpx"
	"juju/internal/ingest"
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
	Queue     queue.Queue
	Blobs     *blob.Store
	MaxUpload int64
	Auth      *auth.Service
	Alerts    *alert.Notifier
	Config    config.Config
	Log       *slog.Logger
}

func Routes(d Deps) http.Handler {
	pool, st, q, blobs, maxUpload, log := d.Pool, d.Store, d.Queue, d.Blobs, d.MaxUpload, d.Log
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
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("OPTIONS /api/v1/projects/{id}/events", ingest.Preflight)
	mux.Handle("POST /api/v1/projects/{id}/events", &ingest.Handler{
		Keys: st, Queue: q, Log: log, Now: time.Now,
	})
	(&api.API{Store: st, Blobs: blobs, MaxUpload: maxUpload, Log: log}).Register(mux)
	(&ui.API{Store: st, Auth: d.Auth, Alerts: d.Alerts, PublicURL: d.Config.PublicURL, AllowSignup: d.Config.AllowSignup, Log: log}).Register(mux)
	// Everything else is the dashboard. More specific patterns (above) win,
	// and an unknown /api/ path must be a JSON 404, not the app's HTML.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "no such API route")
	})
	mux.Handle("/", web.Handler())
	return mux
}

// Run serves HTTP and runs workers until ctx is cancelled (Ctrl-C/SIGTERM),
// then shuts down in an order that loses no accepted events:
//
//  1. stop accepting HTTP requests and finish in-flight ones
//  2. close the queue: nothing can send to it any more
//  3. let workers drain what is buffered
//
// Doing 2 before 1 would panic: a handler sending on a closed channel.
func Run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) error {
	st := store.New(pool)
	q := queue.NewChan(cfg.QueueSize)
	blobs, err := blob.New(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return err
	}
	sym := &symbolicate.Service{Files: symbolicate.StoredFiles{Store: st, Blobs: blobs}, Log: log}
	alerts := &alert.Notifier{
		Settings:          st,
		SMTP:              alert.SMTPConfig{Addr: cfg.SMTPAddr, Username: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.SMTPFrom},
		PublicURL:         cfg.PublicURL,
		AllowHTTPWebhooks: cfg.AllowHTTPWebhooks,
		Log:               log,
	}
	alerts.Start(2)
	authSvc := &auth.Service{Store: st, SecureCookies: strings.HasPrefix(cfg.PublicURL, "https://")}

	workersDone := make(chan struct{})
	go func() {
		// Workers get a fresh context, not ctx: they must keep writing
		// while draining after ctx is cancelled.
		worker.Run(context.Background(), q.Events(), worker.Deps{Store: st, Symbolicator: sym, Alerter: alerts, Log: log}, cfg.Workers)
		close(workersDone)
	}()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           Routes(Deps{Pool: pool, Store: st, Queue: q, Blobs: blobs, MaxUpload: int64(cfg.MaxUploadMB) << 20, Auth: authSvc, Alerts: alerts, Config: cfg, Log: log}),
		ReadHeaderTimeout: 5 * time.Second,  // slow clients can't hold connections open forever
		ReadTimeout:       30 * time.Second, // a 1 MB body on a bad mobile network
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		q.Close()
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	log.Info("juju listening", "addr", ln.Addr().String(), "workers", cfg.Workers, "queue_size", cfg.QueueSize)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		q.Close()
		<-workersDone
		alerts.Stop()
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "queued_events", q.Len())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http shutdown", "err", err)
	}
	q.Close()
	<-workersDone
	alerts.Stop() // after the workers: they may have just queued alerts
	log.Info("shutdown complete")
	return nil
}
