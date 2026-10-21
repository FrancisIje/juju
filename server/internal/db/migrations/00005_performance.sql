-- +goose Up
-- App sessions, for the crash-free rate per release. One row per session,
-- not a running count per hour: a session's state changes after it starts
-- (it can crash later), and SDKs resend updates after a failed send. A row
-- keyed by the session's id absorbs both: the next update overwrites the
-- state, and a repeat changes nothing. Counters can't do that, because
-- moving a session from "ok" to "crashed" needs to know it was counted as ok.
--
-- Named app_sessions because "sessions" already holds dashboard logins.
CREATE TABLE app_sessions (
    project_id  BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    session_id  UUID        NOT NULL,
    release     TEXT        NOT NULL DEFAULT '',
    environment TEXT        NOT NULL,
    platform    TEXT        NOT NULL,
    user_key    TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL CHECK (status IN ('ok', 'exited', 'crashed')),
    -- The highest update counter seen. Updates with a lower one arrived
    -- late and don't change the status.
    seq         BIGINT      NOT NULL,
    errors      INT         NOT NULL DEFAULT 0,
    started_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT,
    PRIMARY KEY (project_id, session_id)
);
CREATE INDEX app_sessions_started ON app_sessions (project_id, started_at DESC);

-- Transactions: page loads, app starts, screen loads. The spans stay in the
-- JSON payload; the columns are what the summary queries filter and sort on.
CREATE TABLE transactions (
    project_id  BIGINT           NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    event_id    UUID             NOT NULL,
    name        TEXT             NOT NULL,
    op          TEXT             NOT NULL,
    status      TEXT             NOT NULL,
    release     TEXT             NOT NULL DEFAULT '',
    environment TEXT             NOT NULL,
    platform    TEXT             NOT NULL,
    started_at  TIMESTAMPTZ      NOT NULL,
    duration_ms DOUBLE PRECISION NOT NULL,
    payload     JSONB            NOT NULL,
    PRIMARY KEY (project_id, event_id)
);
CREATE INDEX transactions_started ON transactions (project_id, started_at DESC);
CREATE INDEX transactions_name ON transactions (project_id, name, started_at DESC);

-- +goose Down
DROP TABLE transactions;
DROP TABLE app_sessions;
