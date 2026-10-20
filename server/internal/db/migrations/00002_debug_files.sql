-- +goose Up
-- Files that turn minified or obfuscated stack frames back into source
-- locations. The bytes live on disk (blob store); this table is the index.
CREATE TABLE debug_files (
    id         BIGSERIAL PRIMARY KEY,
    project_id BIGINT      NOT NULL REFERENCES projects (id),
    -- sourcemap: JavaScript (web, React Native), looked up by release + bundle name
    -- dart_symbols: Flutter --split-debug-info, looked up by build id
    kind       TEXT        NOT NULL CHECK (kind IN ('sourcemap', 'dart_symbols')),
    release    TEXT        NOT NULL DEFAULT '',
    -- sourcemap: the bundle file it describes, e.g. "app.min.js".
    -- dart_symbols: the build id read from the file itself.
    name       TEXT        NOT NULL,
    sha256     TEXT        NOT NULL,
    size       BIGINT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Uploading the same name again for a release replaces it.
    UNIQUE (project_id, kind, release, name)
);

-- +goose Down
DROP TABLE debug_files;
