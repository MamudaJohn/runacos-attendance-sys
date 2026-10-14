-- +goose Up
CREATE TABLE resources (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id uuid NOT NULL REFERENCES courses(id) ON DELETE CASCADE,
    uploaded_by uuid NOT NULL REFERENCES users(id),
    title text NOT NULL CHECK (btrim(title) <> ''),
    original_name text NOT NULL CHECK (btrim(original_name) <> ''),
    storage_key text NOT NULL UNIQUE CHECK (btrim(storage_key) <> ''),
    mime_type text NOT NULL CHECK (mime_type IN ('application/pdf', 'text/plain')),
    size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 5242880),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX resources_course_id_idx ON resources(course_id, created_at, id);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id uuid REFERENCES users(id) ON DELETE SET NULL,
    action text NOT NULL CHECK (btrim(action) <> ''),
    target_type text NOT NULL CHECK (btrim(target_type) <> ''),
    target_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_events_actor_id_idx ON audit_events(actor_id, created_at);
CREATE INDEX audit_events_target_idx ON audit_events(target_type, target_id, created_at);

UPDATE app_metadata SET value = '1b', updated_at = now() WHERE key = 'schema_phase';

-- +goose Down
DROP TABLE audit_events;
DROP TABLE resources;
UPDATE app_metadata SET value = '1a', updated_at = now() WHERE key = 'schema_phase';
