// Command juju is the juju server and its admin tools, in one binary:
//
//	juju serve                              run ingest, API and workers
//	juju migrate up|down|status             manage the database schema
//	juju project create -org acme -name web create a project and print its keys
//	juju upload -kind sourcemap -release R dir/  upload debug files for symbolication
//	juju user create -email E -org O         add a dashboard account (asks for a password)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"juju/internal/auth"
	"juju/internal/config"
	"juju/internal/db"
	"juju/internal/server"
	"juju/internal/store"
)

const usage = `usage:
  juju serve [-migrate=true]
  juju migrate up|down|status
  juju project create -org NAME -name NAME
  juju upload -kind sourcemap|dart_symbols [-release R] PATH...
  juju user create -email EMAIL -org NAME [-name NAME]`

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
	case "user":
		if len(args) < 2 || args[1] != "create" {
			return errors.New(usage)
		}
		return createUser(ctx, cfg, args[2:])
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

// createUser adds a dashboard account from the command line: how you add
// people when sign-up is closed (the default after the first account).
// The password is read from JUJU_PASSWORD or the first line of stdin, never
// from a flag, because flags end up in shell history and process lists.
func createUser(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	email := fs.String("email", "", "email address to log in with")
	name := fs.String("name", "", "display name")
	org := fs.String("org", "", "organization to make them an owner of (created if needed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *org == "" {
		return errors.New("both -email and -org are required")
	}
	password := os.Getenv("JUJU_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no password given (type it, or set JUJU_PASSWORD)")
		}
		password = strings.TrimRight(line, "\r\n")
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		return err
	}
	u, err := store.New(pool).CreateUser(ctx, auth.NormalizeEmail(*email), *name, hash, *org, store.JoinOrCreate)
	if err != nil {
		return err
	}
	fmt.Printf("created %s (id %d), owner of %q\n", u.Email, u.ID, *org)
	return nil
}
