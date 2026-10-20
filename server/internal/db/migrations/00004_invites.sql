-- +goose Up
-- Invite links: an owner creates one, sends it, and whoever opens it joins
-- the organization. Only the SHA-256 of the link's token is stored, like
-- sessions, so a database leak doesn't hand out working invites. Each link
-- works once and expires.
CREATE TABLE invites (
    id              BIGSERIAL PRIMARY KEY,
    organization_id BIGINT      NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    token_hash      BYTEA       NOT NULL UNIQUE,
    role            TEXT        NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    created_by      BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    used_by         BIGINT      REFERENCES users (id) ON DELETE SET NULL
);
CREATE INDEX invites_org ON invites (organization_id);

-- +goose Down
DROP TABLE invites;
