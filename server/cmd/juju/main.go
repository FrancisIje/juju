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
	"strconv"
	"strings"
	"syscall"
	"time"

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
  juju project limits -id ID [-rate N|default] [-quota N|none]
  juju sourcemaps inject PATH...
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
	if args[0] == "sourcemaps" {
		return sourcemaps(args[1:]) // works on files only, like upload
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
		if len(args) < 2 {
			return errors.New(usage)
		}
		switch args[1] {
		case "create":
			return createProject(ctx, cfg, args[2:])
		case "limits":
			return projectLimits(ctx, cfg, args[2:])
		}
		return errors.New(usage)
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

// projectLimits shows or changes a project's rate limit and monthly quota.
// A flag left out keeps its current value; with no flags it just shows them.
func projectLimits(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("project limits", flag.ContinueOnError)
	id := fs.Int64("id", 0, "project id")
	rate := fs.String("rate", "", `events per second, 0 for unlimited, or "default" for JUJU_RATE_LIMIT`)
	quota := fs.String("quota", "", `events per calendar month (UTC), or "none"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id <= 0 {
		return errors.New("-id is required")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, "up"); err != nil {
		return err
	}
	st := store.New(pool)
	p, err := st.ProjectIngest(ctx, *id)
	if err != nil {
		return fmt.Errorf("project %d: %w", *id, err)
	}
	r, q := p.RateLimit, p.MonthlyQuota
	if set["rate"] {
		if r, err = parseLimit[int](*rate, "default"); err != nil {
			return fmt.Errorf("-rate: %w", err)
		}
	}
	if set["quota"] {
		if q, err = parseLimit[int64](*quota, "none"); err != nil {
			return fmt.Errorf("-quota: %w", err)
		}
	}
	if set["rate"] || set["quota"] {
		if err := st.SetProjectLimits(ctx, *id, r, q); err != nil {
			return err
		}
	}
	used, err := st.MonthUsage(ctx, *id, store.MonthStart(time.Now()))
	if err != nil {
		return err
	}
	rateText := fmt.Sprintf("default (JUJU_RATE_LIMIT=%d)", cfg.RateLimit)
	if r != nil {
		rateText = strconv.Itoa(*r) + " events/s"
		if *r == 0 {
			rateText = "unlimited"
		}
	}
	quotaText := "none"
	if q != nil {
		quotaText = strconv.FormatInt(*q, 10) + " events/month"
	}
	fmt.Printf("project %d\n  rate limit: %s\n  quota:      %s\n  used this month: %d\n", *id, rateText, quotaText, used)
	fmt.Println("Running servers pick up changes within 30 seconds.")
	return nil
}

// parseLimit reads a non-negative number, or the word that means "not set"
// (nil).
func parseLimit[T int | int64](s, unset string) (*T, error) {
	if s == unset {
		return nil, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("%q is not a number >= 0 or %q", s, unset)
	}
	v := T(n)
	return &v, nil
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
