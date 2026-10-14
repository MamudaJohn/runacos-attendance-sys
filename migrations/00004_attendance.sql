-- +goose Up
-- This milestone accepts possession of a short-lived token for lecturer review.
-- It does not claim acoustic decoding or proof of physical presence.
CREATE TABLE attendance_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    course_id uuid NOT NULL REFERENCES courses(id),
    room_id uuid NOT NULL REFERENCES rooms(id),
    opened_by uuid NOT NULL REFERENCES users(id),
    title text NOT NULL CHECK (btrim(title) <> '' AND octet_length(title) <= 200),
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'open', 'closed')),
    duration_minutes integer NOT NULL CHECK (duration_minutes BETWEEN 1 AND 180),
    starts_at timestamptz,
    ends_at timestamptz,
    policy_version text NOT NULL DEFAULT 'token-review-v1' CHECK (policy_version = 'token-review-v1'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((starts_at IS NULL AND ends_at IS NULL AND status IN ('draft', 'closed'))
        OR (starts_at IS NOT NULL AND ends_at IS NOT NULL AND ends_at > starts_at AND status IN ('open', 'closed')))
);
CREATE INDEX attendance_sessions_course_idx ON attendance_sessions(course_id, created_at DESC, id);

CREATE TABLE acoustic_challenges (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    generation integer NOT NULL CHECK (generation > 0),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (session_id, generation),
    UNIQUE (id, session_id)
);
CREATE INDEX acoustic_challenges_session_idx ON acoustic_challenges(session_id, created_at DESC);

CREATE TABLE submission_nonces (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    student_id uuid NOT NULL REFERENCES users(id),
    nonce_hash text NOT NULL UNIQUE CHECK (nonce_hash ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (id, session_id, student_id)
);
CREATE INDEX submission_nonces_owner_idx ON submission_nonces(session_id, student_id, created_at DESC);

CREATE TABLE verification_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    student_id uuid NOT NULL REFERENCES users(id),
    challenge_id uuid,
    decision text NOT NULL CHECK (decision IN ('pending', 'rejected')),
    reason_code text NOT NULL CHECK (btrim(reason_code) <> ''),
    policy_version text NOT NULL CHECK (policy_version = 'token-review-v1'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (challenge_id, session_id) REFERENCES acoustic_challenges(id, session_id),
    UNIQUE (id, session_id, student_id)
);
CREATE INDEX verification_attempts_owner_idx ON verification_attempts(session_id, student_id, created_at DESC);

CREATE TABLE attendance_records (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    student_id uuid NOT NULL REFERENCES users(id),
    status text NOT NULL CHECK (status IN ('pending', 'present', 'rejected')),
    source text NOT NULL CHECK (source IN ('challenge', 'manual')),
    reason_code text NOT NULL CHECK (btrim(reason_code) <> ''),
    source_attempt_id uuid,
    reviewed_by uuid REFERENCES users(id),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (session_id, student_id),
    FOREIGN KEY (source_attempt_id, session_id, student_id)
        REFERENCES verification_attempts(id, session_id, student_id),
    CHECK ((source = 'challenge' AND source_attempt_id IS NOT NULL AND reviewed_by IS NULL AND status IN ('pending', 'rejected'))
        OR (source = 'manual' AND reviewed_by IS NOT NULL AND status IN ('present', 'rejected')))
);

CREATE TABLE attendance_reviews (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    student_id uuid NOT NULL REFERENCES users(id),
    actor_id uuid NOT NULL REFERENCES users(id),
    decision text NOT NULL CHECK (decision IN ('present', 'rejected')),
    reason text NOT NULL CHECK (btrim(reason) <> '' AND octet_length(reason) <= 500),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX attendance_reviews_owner_idx ON attendance_reviews(session_id, student_id, created_at DESC, id);

UPDATE app_metadata SET value = '1c', updated_at = now() WHERE key = 'schema_phase';

-- +goose Down
DROP TABLE attendance_reviews;
DROP TABLE attendance_records;
DROP TABLE verification_attempts;
DROP TABLE submission_nonces;
DROP TABLE acoustic_challenges;
DROP TABLE attendance_sessions;
UPDATE app_metadata SET value = '1b', updated_at = now() WHERE key = 'schema_phase';
