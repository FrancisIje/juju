-- +goose Up
-- People who run this juju server, as opposed to people who use it. Set only
-- from the server's shell (`juju user superadmin`), never over HTTP, and not
-- from an email allow-list in config: sign-up doesn't verify email addresses,
-- so anyone could register an allow-listed address first.
ALTER TABLE users ADD COLUMN is_superadmin BOOLEAN NOT NULL DEFAULT false;

-- When ingest last accepted an event for this project, on the server's clock.
-- Ingest already writes this row in batches every few seconds, so this adds
-- no work per event, and it sees every kind of event, including analytics
-- ones, which live in ClickHouse. NULL for rows written before this column.
ALTER TABLE project_usage ADD COLUMN last_accepted_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE project_usage DROP COLUMN last_accepted_at;
ALTER TABLE users DROP COLUMN is_superadmin;
