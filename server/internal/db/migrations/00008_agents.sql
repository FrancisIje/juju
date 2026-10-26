-- +goose Up
-- Which commit each release was built from. An agent fixing a crash has to
-- read the code that crashed, not today's code, and the release name alone
-- ("web@2.3.1") doesn't say where that is. Set by `juju release` from CI.
CREATE TABLE releases (
    project_id BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    commit_sha TEXT        NOT NULL DEFAULT '',
    repository TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, name)
);

-- Links attached to an issue, such as the pull request that fixes it, so
-- people and agents can see a fix is already under way.
CREATE TABLE issue_links (
    id         BIGSERIAL PRIMARY KEY,
    issue_id   BIGINT      NOT NULL REFERENCES issues (id) ON DELETE CASCADE,
    url        TEXT        NOT NULL,
    title      TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issue_id, url)
);

-- A generic webhook: juju posts JSON about new issues and regressions, signed
-- with webhook_secret so the receiver can check it came from juju.
ALTER TABLE alert_settings
    ADD COLUMN webhook_url    TEXT NOT NULL DEFAULT '',
    ADD COLUMN webhook_secret TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE alert_settings DROP COLUMN webhook_secret, DROP COLUMN webhook_url;
DROP TABLE issue_links;
DROP TABLE releases;
