// Command juju is the juju server and its admin tools, in one binary:
//
//	juju serve                              run ingest, API and workers
//	juju migrate up|down|status             manage the database schema
//	juju project create -org acme -name web create a project and print its keys
//	juju upload -kind sourcemap -release R dir/  upload debug files for symbolication
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"juju/internal/config"
	"juju/internal/db"
	"juju/internal/server"
	"juju/internal/store"
)

const usage = `usage:
  juju serve [-migrate=true]
  juju migrate up|down|status
  juju project create -org NAME -name NAME
  juju upload -kind sourcemap|dart_symbols [-release R] PATH...`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	// upload talks to a server over HTTP; it needs no database settings.
	if args[0] == "upload" {
		return upload(args[1:])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// JSON logs are easy for machines (log search, alerting) to parse.
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	// ctx is cancelled on Ctrl-C (SIGINT) or SIGTERM (what Docker and
	// Kubernetes send on stop), which starts a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "serve":
		return serve(ctx, cfg, log, args[1:])
	case "migrate":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return migrate(ctx, cfg, args[1])
	case "project":
		if len(args) < 2 || args[1] != "create" {
			return errors.New(usage)
		}
		return createProject(ctx, cfg, args[2:])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	autoMigrate := fs.Bool("migrate", true, "apply pending migrations before serving")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if *autoMigrate {
		if err := db.Migrate(ctx, pool, "up"); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return server.Run(ctx, cfg, pool, log)
}

func migrate(ctx context.Context, cfg config.Config, command string) error {
	switch command {
	case "up", "down", "status":
	default:
		return fmt.Errorf("unknown migrate command %q (use up, down or status)", command)
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return db.Migrate(ctx, pool, command)
}

func createProject(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("project create", flag.ContinueOnError)
	org := fs.String("org", "", "organization name (created if it doesn't exist)")
	name := fs.String("name", "", "project name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *name == "" {
		return errors.New("both -org and -name are required")
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		return err
	}
	p, err := store.New(pool).CreateProject(ctx, *org, *name)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Save the secret key now: it is stored only as a hash and can't be shown again.")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}
