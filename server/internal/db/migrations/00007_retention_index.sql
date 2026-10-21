-- +goose NO TRANSACTION
-- +goose Up
-- The retention job deletes by age across all projects. The existing
-- indexes start with project_id, which a range over time alone can't use,
-- so without these each run would read the whole table.
--
-- CONCURRENTLY builds the index without blocking writes, so ingest keeps
-- storing events during a deploy; a plain CREATE INDEX would lock the
-- table for as long as the build takes. It can't run inside a transaction,
-- hence NO TRANSACTION above, and IF NOT EXISTS so a retry after a failed
-- deploy is safe.
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_received_at ON events (received_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS app_sessions_started_at ON app_sessions (started_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS transactions_started_at ON transactions (started_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS transactions_started_at;
DROP INDEX CONCURRENTLY IF EXISTS app_sessions_started_at;
DROP INDEX CONCURRENTLY IF EXISTS events_received_at;
