// Package analytics stores product analytics events in ClickHouse and
// answers the questions the dashboard asks of them: trends, funnels and
// retention.
//
// Why ClickHouse: analytics events outnumber errors by 100x or more, and the
// questions are "count these over time, grouped by that". A column store
// reads only the columns a query names, compressed, which keeps those
// queries fast long after Postgres would slow down.
//
// It talks to ClickHouse over its HTTP interface with net/http: no driver to
// learn or upgrade, and every value goes in as a query parameter
// ({name:Type}), never pasted into SQL text, so input can't change a query.
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal ClickHouse HTTP client.
type Client struct {
	URL      string // e.g. http://127.0.0.1:8123
	User     string
	Password string
	Database string // default "juju"
	// RetentionDays: analytics events older than this are deleted by
	// ClickHouse itself (a table TTL, set by Migrate). 0 keeps them forever.
	RetentionDays int
	HTTP          *http.Client
}

func (c *Client) db() string {
	if c.Database == "" {
		return "juju"
	}
	return c.Database
}

func (c *Client) do(ctx context.Context, query string, params map[string]string, body io.Reader) ([]byte, error) {
	return c.doIn(ctx, c.db(), query, params, body)
}

// doIn runs a request with db as the current database. CREATE DATABASE has
// to run in one that already exists ("default"): naming the one being
// created would fail, because it doesn't exist yet.
func (c *Client) doIn(ctx context.Context, db, query string, params map[string]string, body io.Reader) ([]byte, error) {
	q := url.Values{}
	q.Set("database", db)
	if body != nil {
		q.Set("query", query) // INSERT ... FORMAT JSONEachRow; the rows are the body
	}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	// Timestamps written like 2026-10-20T10:00:00.123Z are accepted.
	q.Set("date_time_input_format", "best_effort")
	// Every date function works in UTC, whatever timezone the ClickHouse
	// server runs in. Without this, toDateTime(toDate(t)) gave midnight in
	// the server's zone: on a server at UTC+1 each day's bucket started at
	// 23:00 UTC the day before, and every trend came back empty.
	q.Set("session_timezone", "UTC")
	if body == nil {
		body = strings.NewReader(query)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.Header.Set("X-ClickHouse-User", c.User)
		req.Header.Set("X-ClickHouse-Key", c.Password)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return nil, fmt.Errorf("clickhouse: %s: %s", res.Status, msg)
	}
	return out, nil
}

// Exec runs a statement that returns nothing (CREATE, ALTER).
func (c *Client) Exec(ctx context.Context, query string) error {
	_, err := c.do(ctx, query, nil, nil)
	return err
}

// Ping checks ClickHouse answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, "SELECT 1", nil, nil)
	return err
}

// Insert writes rows as JSON, one object per line, in one request: one
// insert per batch, which is how ClickHouse likes to be written to.
func (c *Client) Insert(ctx context.Context, table string, rows []any) error {
	if len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	_, err := c.do(ctx, "INSERT INTO "+table+" FORMAT JSONEachRow", nil, &buf)
	return err
}

// Query runs a SELECT and decodes each result row into a T. params fill
// {name:Type} placeholders.
func Query[T any](ctx context.Context, c *Client, query string, params map[string]string) ([]T, error) {
	out, err := c.do(ctx, query+" FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers = 0", params, nil)
	if err != nil {
		return nil, err
	}
	var rows []T
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var r T
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("clickhouse: decode row: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, nil
}
