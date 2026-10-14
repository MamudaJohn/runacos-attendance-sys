-- +goose Up
-- A minimal foundation marker; domain tables belong to Phase 1.
CREATE TABLE app_metadata (
    key text PRIMARY KEY,
    value text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO app_metadata (key, value) VALUES ('schema_phase', '0');

-- +goose Down
DROP TABLE app_metadata;
