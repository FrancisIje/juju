-- +goose Up
-- People who use the dashboard. Passwords are stored as bcrypt hashes.
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL DEFAULT '',
    password_hash TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    user_id         BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    role            TEXT        NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, organization_id)
);

-- Login sessions. Only the SHA-256 of the cookie's token is stored, so a
-- database leak doesn't hand out working sessions.
CREATE TABLE sessions (
    token_hash BYTEA       PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

-- The issue workflow: unresolved -> resolved (or ignored). A resolved issue
-- that happens again is a regression and goes back to unresolved.
ALTER TABLE issues
    ADD COLUMN platform          TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_release      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN user_count        BIGINT      NOT NULL DEFAULT 0,
    ADD COLUMN status_changed_at TIMESTAMPTZ,
    ADD CONSTRAINT issues_status_check CHECK (status IN ('unresolved', 'resolved', 'ignored'));

-- Which app users hit an issue, so "affected users" is a count of distinct
-- people, not of events.
CREATE TABLE issue_users (
    issue_id BIGINT NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    user_key TEXT   NOT NULL,
    PRIMARY KEY (issue_id, user_key)
);

-- What happened to an issue and who did it, for the issue page's history.
CREATE TABLE issue_activity (
    id         BIGSERIAL PRIMARY KEY,
    issue_id   BIGINT      NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL CHECK (kind IN ('created', 'resolved', 'ignored', 'reopened', 'regressed')),
    user_id    BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    release    TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX issue_activity_issue ON issue_activity (issue_id, created_at DESC);

CREATE TABLE alert_settings (
    project_id        BIGINT      PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    slack_webhook_url TEXT        NOT NULL DEFAULT '',
    email_to          TEXT[]      NOT NULL DEFAULT '{}',
    on_new_issue      BOOLEAN     NOT NULL DEFAULT true,
    on_regression     BOOLEAN     NOT NULL DEFAULT true,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Every alert sent or attempted, so "did the alert go out?" has an answer.
CREATE TABLE alert_deliveries (
    id         BIGSERIAL PRIMARY KEY,
    project_id BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    issue_id   BIGINT      REFERENCES issues (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL, -- new_issue, regression, test
    channel    TEXT        NOT NULL, -- slack, email
    ok         BOOLEAN     NOT NULL,
    error      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_deliveries_project ON alert_deliveries (project_id, created_at DESC);

-- +goose Down
DROP TABLE alert_deliveries;
DROP TABLE alert_settings;
DROP TABLE issue_activity;
DROP TABLE issue_users;
ALTER TABLE issues
    DROP CONSTRAINT issues_status_check,
    DROP COLUMN status_changed_at,
    DROP COLUMN user_count,
    DROP COLUMN last_release,
    DROP COLUMN platform;
DROP TABLE sessions;
DROP TABLE memberships;
DROP TABLE users;
