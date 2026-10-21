-- +goose Up
-- Per-project limits, checked by ingest. NULL means "use the server's
-- default" (JUJU_RATE_LIMIT) for the rate, and "no quota" for the quota,
-- so existing projects keep working unchanged.
ALTER TABLE projects
    ADD COLUMN rate_limit    INTEGER CHECK (rate_limit >= 0),   -- events per second; 0 = unlimited
    ADD COLUMN monthly_quota BIGINT  CHECK (monthly_quota >= 0); -- events per calendar month (UTC)

-- Events accepted per project per month, for quotas. Ingest adds to it in
-- batches every few seconds rather than once per request, so this table
-- isn't a hot spot. One row per month also gives usage history for free.
CREATE TABLE project_usage (
    project_id BIGINT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    month      DATE   NOT NULL, -- first day of the month, UTC
    accepted   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, month)
);

-- +goose Down
DROP TABLE project_usage;
ALTER TABLE projects DROP COLUMN monthly_quota, DROP COLUMN rate_limit;
