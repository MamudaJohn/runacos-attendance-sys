-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE CHECK (email = lower(email) AND email = btrim(email) AND email <> ''),
    name text NOT NULL CHECK (btrim(name) <> ''),
    password_hash text NOT NULL CHECK (password_hash <> ''),
    role text NOT NULL CHECK (role IN ('admin', 'lecturer', 'student')),
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash text PRIMARY KEY CHECK (token_hash <> ''),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE courses (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code text NOT NULL UNIQUE CHECK (btrim(code) <> ''),
    title text NOT NULL CHECK (btrim(title) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE course_lecturers (
    course_id uuid NOT NULL REFERENCES courses(id) ON DELETE CASCADE,
    lecturer_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (course_id, lecturer_id)
);
CREATE INDEX course_lecturers_lecturer_id_idx ON course_lecturers(lecturer_id);

CREATE TABLE enrollments (
    course_id uuid NOT NULL REFERENCES courses(id) ON DELETE CASCADE,
    student_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'withdrawn')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (course_id, student_id)
);
CREATE INDEX enrollments_student_id_idx ON enrollments(student_id);

CREATE TABLE rooms (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE CHECK (btrim(name) <> ''),
    kind text NOT NULL CHECK (kind IN ('room', 'theatre')),
    capacity integer NOT NULL CHECK (capacity > 0),
    profile_version text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

UPDATE app_metadata SET value = '1a', updated_at = now() WHERE key = 'schema_phase';

-- +goose Down
DROP TABLE rooms;
DROP TABLE enrollments;
DROP TABLE course_lecturers;
DROP TABLE courses;
DROP TABLE sessions;
DROP TABLE users;
UPDATE app_metadata SET value = '0', updated_at = now() WHERE key = 'schema_phase';
