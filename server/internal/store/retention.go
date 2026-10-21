package store

import (
	"context"
	"fmt"
	"time"
)

// Deleted counts the rows one retention run removed, per table.
type Deleted map[string]int64

// retentionLock is the Postgres advisory lock id for retention runs (any
// fixed number works; it just has to be the same in every process).
const retentionLock = 0x6a756a75 // "juju"

// DeleteExpired removes data older than the cutoffs, a batch at a time.
//
//   - events, app sessions, transactions, alert deliveries: older than
//     dataBefore. Issues themselves stay, with their counts, so an old bug
//     that comes back is still recognised as a regression.
//   - dashboard logins that have expired, and invites expired over a month.
//
// Small batches keep each DELETE quick: one huge DELETE holds its locks
// and builds up WAL for minutes, while ingest is trying to write to the
// same tables. If another process is already running it (several workers),
// this returns ran=false and does nothing.
func (s *Store) DeleteExpired(ctx context.Context, dataBefore, now time.Time, batch int) (d Deleted, ran bool, err error) {
	// A session-level advisory lock lives on one connection, so hold one
	// connection for the whole run.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, retentionLock).Scan(&ran); err != nil || !ran {
		return nil, false, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, retentionLock)

	d = Deleted{}
	// ctid is a row's physical address: the cheapest way to say "these
	// rows" when a table's key has several columns.
	jobs := []struct{ table, where string }{
		{"events", "received_at < $1"},
		{"app_sessions", "started_at < $1"},
		{"transactions", "started_at < $1"},
		{"alert_deliveries", "created_at < $1"},
	}
	for _, j := range jobs {
		n, err := deleteBatches(ctx, conn.Exec, j.table, j.where, batch, dataBefore)
		d[j.table] = n
		if err != nil {
			return d, true, err
		}
	}
	if d["sessions"], err = deleteBatches(ctx, conn.Exec, "sessions", "expires_at < $1", batch, now); err != nil {
		return d, true, err
	}
	d["invites"], err = deleteBatches(ctx, conn.Exec, "invites", "expires_at < $1", batch, now.AddDate(0, -1, 0))
	return d, true, err
}

// deleteBatches deletes rows matching where ($1 = before), batch rows per
// statement, until fewer than a batch are left.
func deleteBatches[T interface{ RowsAffected() int64 }](ctx context.Context, exec func(context.Context, string, ...any) (T, error), table, where string, batch int, before time.Time) (int64, error) {
	q := fmt.Sprintf(`DELETE FROM %s WHERE ctid = ANY(ARRAY(SELECT ctid FROM %s WHERE %s LIMIT %d))`, table, table, where, batch)
	var total int64
	for {
		tag, err := exec(ctx, q, before)
		if err != nil {
			return total, fmt.Errorf("delete old %s: %w", table, err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
