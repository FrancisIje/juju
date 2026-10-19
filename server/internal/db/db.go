// Package db opens the Postgres connection pool and runs schema migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// The SQL files are compiled into the binary, so a deploy is a single file
// with no "forgot to copy the migrations folder" failure mode.
//
//go:embed migrations/*.sql
var migrations embed.FS

// Open connects a pgx pool and checks the database is reachable.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// Migrate runs a goose command ("up", "down", "status", ...) against the pool.
// goose speaks database/sql, so we wrap the pgx pool in a *sql.DB adapter.
func Migrate(ctx context.Context, pool *pgxpool.Pool, command string) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()
	return runGoose(ctx, sqlDB, command)
}

func runGoose(ctx context.Context, sqlDB *sql.DB, command string) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.RunContext(ctx, command, sqlDB, "migrations")
}
