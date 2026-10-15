-- +goose Up
-- Optional sensor observations are untrusted browser claims, used for research/review.
-- They never convert an attendance submission to present.
CREATE TABLE room_calibrations (
    room_id uuid PRIMARY KEY REFERENCES rooms(id),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    actor_id uuid NOT NULL REFERENCES users(id),
    spectral_features jsonb NOT NULL CHECK (jsonb_typeof(spectral_features) = 'array' AND jsonb_array_length(spectral_features) = 8),
    latitude double precision,
    longitude double precision,
    radius_meters double precision,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((latitude IS NULL AND longitude IS NULL AND radius_meters IS NULL)
       OR (latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180 AND radius_meters BETWEEN 5 AND 10000))
);
CREATE TABLE attendance_evidence (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    student_id uuid NOT NULL REFERENCES users(id),
    capture_method text NOT NULL CHECK (capture_method IN ('manual', 'acoustic', 'qr', 'ble')),
    observation jsonb NOT NULL CHECK (jsonb_typeof(observation) = 'object' AND octet_length(observation::text) <= 4096),
    comparison jsonb NOT NULL CHECK (jsonb_typeof(comparison) = 'object' AND octet_length(comparison::text) <= 4096),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL DEFAULT (clock_timestamp() + interval '7 days'),
    UNIQUE(id,session_id,student_id)
);
CREATE INDEX attendance_evidence_owner_idx ON attendance_evidence(session_id,student_id,created_at DESC);
CREATE INDEX attendance_evidence_expiry_idx ON attendance_evidence(expires_at);
ALTER TABLE verification_attempts ADD COLUMN capture_method text NOT NULL DEFAULT 'manual'
    CHECK (capture_method IN ('manual', 'acoustic', 'qr', 'ble'));
ALTER TABLE verification_attempts ADD COLUMN evidence_id uuid;
ALTER TABLE verification_attempts ADD CONSTRAINT verification_attempts_evidence_owner_fk
    FOREIGN KEY(evidence_id,session_id,student_id) REFERENCES attendance_evidence(id,session_id,student_id);
CREATE UNIQUE INDEX verification_attempts_evidence_use_idx ON verification_attempts(evidence_id) WHERE evidence_id IS NOT NULL;
UPDATE app_metadata SET value = '2a-experimental', updated_at = now() WHERE key = 'schema_phase';

-- +goose Down
ALTER TABLE verification_attempts DROP CONSTRAINT verification_attempts_evidence_owner_fk;
ALTER TABLE verification_attempts DROP COLUMN evidence_id;
ALTER TABLE verification_attempts DROP COLUMN capture_method;
DROP TABLE attendance_evidence;
DROP TABLE room_calibrations;
UPDATE app_metadata SET value = '1c', updated_at = now() WHERE key = 'schema_phase';
