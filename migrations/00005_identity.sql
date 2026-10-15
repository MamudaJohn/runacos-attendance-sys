-- +goose Up
-- Store public credential records and server-held, one-use WebAuthn ceremonies.
-- No fingerprints, face images, biometric templates or private keys are stored.
ALTER TABLE attendance_sessions ADD COLUMN identity_required boolean NOT NULL DEFAULT false;
CREATE TABLE identity_credentials (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    rp_id text NOT NULL CHECK (btrim(rp_id) <> '' AND octet_length(rp_id) <= 253),
    credential_id bytea NOT NULL CHECK (octet_length(credential_id) BETWEEN 1 AND 1024),
    credential_data jsonb NOT NULL CHECK (jsonb_typeof(credential_data) = 'object'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    UNIQUE (rp_id, credential_id),
    UNIQUE (id, user_id)
);
CREATE INDEX identity_credentials_owner_idx ON identity_credentials(user_id, rp_id, created_at);

CREATE TABLE identity_ceremonies (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    rp_id text NOT NULL CHECK (btrim(rp_id) <> ''),
    auth_session_hash text NOT NULL CHECK (auth_session_hash ~ '^[0-9a-f]{64}$'),
    purpose text NOT NULL CHECK (purpose IN ('registration', 'attendance')),
    session_id uuid REFERENCES attendance_sessions(id),
    nonce_hash text CHECK (nonce_hash ~ '^[0-9a-f]{64}$'),
    session_data jsonb NOT NULL CHECK (jsonb_typeof(session_data) = 'object'),
    expires_at timestamptz NOT NULL DEFAULT clock_timestamp() + interval '5 minutes',
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((purpose = 'registration' AND session_id IS NULL AND nonce_hash IS NULL)
        OR (purpose = 'attendance' AND session_id IS NOT NULL AND nonce_hash IS NOT NULL))
);
CREATE INDEX identity_ceremonies_owner_idx ON identity_ceremonies(user_id, created_at DESC);

CREATE TABLE identity_proofs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id),
    session_id uuid NOT NULL REFERENCES attendance_sessions(id),
    credential_id uuid NOT NULL,
    nonce_hash text NOT NULL CHECK (nonce_hash ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL DEFAULT clock_timestamp() + interval '2 minutes',
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (credential_id, user_id) REFERENCES identity_credentials(id, user_id)
);
CREATE INDEX identity_proofs_owner_idx ON identity_proofs(user_id, session_id, created_at DESC);
ALTER TABLE identity_proofs ADD UNIQUE (id, session_id, user_id);
ALTER TABLE verification_attempts ADD COLUMN identity_proof_id uuid;
ALTER TABLE verification_attempts ADD CONSTRAINT verification_attempts_identity_fk
    FOREIGN KEY (identity_proof_id, session_id, student_id) REFERENCES identity_proofs(id, session_id, user_id);
CREATE UNIQUE INDEX verification_attempts_identity_once_idx ON verification_attempts(identity_proof_id) WHERE identity_proof_id IS NOT NULL;

-- +goose Down
ALTER TABLE verification_attempts DROP COLUMN identity_proof_id;
DROP TABLE identity_proofs;
DROP TABLE identity_ceremonies;
DROP TABLE identity_credentials;
ALTER TABLE attendance_sessions DROP COLUMN identity_required;
