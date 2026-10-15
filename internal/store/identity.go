package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrIdentityRejected = errors.New("identity confirmation is invalid, expired or already used")
	ErrIdentityLimit    = errors.New("identity ceremony or credential limit reached")
)

// IdentityCredential stores the whole versioned WebAuthn Credential JSON record,
// including the public key, flags, counter and durable extension output.
type IdentityCredential struct {
	ID           string          `json:"id"`
	CredentialID []byte          `json:"-"`
	Data         json.RawMessage `json:"-"`
	CreatedAt    time.Time       `json:"created_at"`
	LastUsedAt   *time.Time      `json:"last_used_at"`
}

type IdentityCeremony struct {
	ID, NonceHash string
	Data          json.RawMessage
}

type IdentityProof struct {
	ID        string    `json:"proof_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LockIdentityActorTx applies the same lock order and current membership checks
// as the attendance verifier. Registration is restricted to active students.
func (s *Store) LockIdentityActorTx(ctx context.Context, tx pgx.Tx, actor User, sessionID string) (User, error) {
	if sessionID != "" {
		session, err := lockAttendanceStudent(ctx, tx, actor, sessionID)
		if err != nil {
			return User{}, err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return User{}, err
		}
		if session.Status != "open" || !now.Before(session.EndsAt) {
			return User{}, ErrAttendanceState
		}
	}
	if _, err := parameterUUID(actor.ID); err != nil {
		return User{}, err
	}
	var current User
	query := `SELECT id::text,email,name,role FROM users WHERE id=$1 AND active AND role='student' FOR SHARE`
	if sessionID == "" {
		query = `SELECT id::text,email,name,role FROM users WHERE id=$1 AND active AND role='student' FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query, actor.ID).Scan(&current.ID, &current.Email, &current.Name, &current.Role)
	return current, err
}

func (s *Store) IdentityCredentialsTx(ctx context.Context, tx pgx.Tx, userID, rpID string) ([]IdentityCredential, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,credential_id,credential_data,created_at,last_used_at
		FROM identity_credentials WHERE user_id=$1 AND rp_id=$2 AND revoked_at IS NULL ORDER BY id FOR UPDATE`, userID, rpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := make([]IdentityCredential, 0)
	for rows.Next() {
		var c IdentityCredential
		if err := rows.Scan(&c.ID, &c.CredentialID, &c.Data, &c.CreatedAt, &c.LastUsedAt); err != nil {
			return nil, err
		}
		credentials = append(credentials, c)
	}
	return credentials, rows.Err()
}

// CheckIdentityNonceTx never permits a proof for another user's nonce, a used
// nonce or an expired nonce. Its owner has already been authorized and locked.
func (s *Store) CheckIdentityNonceTx(ctx context.Context, tx pgx.Tx, userID, sessionID, nonceHash string) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT id::text FROM submission_nonces WHERE student_id=$1 AND session_id=$2
		AND nonce_hash=$3 AND used_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, userID, sessionID, nonceHash).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIdentityRejected
	}
	return err
}

func (s *Store) BeginIdentityCeremonyTx(ctx context.Context, tx pgx.Tx, userID, rpID, authSessionHash, purpose, sessionID, nonceHash string, data []byte) (string, error) {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_ceremonies WHERE user_id=$1 AND created_at>clock_timestamp()-interval '1 minute'`, userID).Scan(&count); err != nil {
		return "", err
	}
	if count >= 10 {
		return "", ErrIdentityLimit
	}
	var session, nonce any
	if sessionID != "" {
		session = sessionID
		nonce = nonceHash
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO identity_ceremonies(user_id,rp_id,auth_session_hash,purpose,session_id,nonce_hash,session_data)
		VALUES($1,$2,$3,$4,$5,$6,$7::jsonb) RETURNING id::text`, userID, rpID, authSessionHash, purpose, session, nonce, data).Scan(&id)
	return id, err
}

func (s *Store) LockIdentityCeremonyTx(ctx context.Context, tx pgx.Tx, userID, rpID, authSessionHash, purpose, sessionID, ceremonyID string) (IdentityCeremony, error) {
	if _, err := parameterUUID(ceremonyID); err != nil {
		return IdentityCeremony{}, ErrIdentityRejected
	}
	var session any
	if sessionID != "" {
		session = sessionID
	}
	var c IdentityCeremony
	var nonce *string
	err := tx.QueryRow(ctx, `SELECT id::text,nonce_hash,session_data FROM identity_ceremonies
		WHERE id=$1 AND user_id=$2 AND rp_id=$3 AND purpose=$4 AND session_id IS NOT DISTINCT FROM $5::uuid
		AND auth_session_hash=$6 AND used_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE`, ceremonyID, userID, rpID, purpose, session, authSessionHash).Scan(&c.ID, &nonce, &c.Data)
	if nonce != nil {
		c.NonceHash = *nonce
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityCeremony{}, ErrIdentityRejected
	}
	return c, err
}

func (s *Store) UseIdentityCeremonyTx(ctx context.Context, tx pgx.Tx, ceremonyID string) error {
	result, err := tx.Exec(ctx, `UPDATE identity_ceremonies SET used_at=clock_timestamp() WHERE id=$1 AND used_at IS NULL`, ceremonyID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrIdentityRejected
	}
	return nil
}

func (s *Store) SaveIdentityCredentialTx(ctx context.Context, tx pgx.Tx, userID, rpID string, credentialID, data []byte) (IdentityCredential, error) {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_credentials WHERE user_id=$1 AND rp_id=$2 AND revoked_at IS NULL`, userID, rpID).Scan(&count); err != nil {
		return IdentityCredential{}, err
	}
	if count >= 8 {
		return IdentityCredential{}, ErrIdentityLimit
	}
	var c IdentityCredential
	err := tx.QueryRow(ctx, `INSERT INTO identity_credentials(user_id,rp_id,credential_id,credential_data)
		VALUES($1,$2,$3,$4::jsonb) RETURNING id::text,created_at`, userID, rpID, credentialID, data).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return IdentityCredential{}, err
	}
	err = auditMutation(ctx, tx, userID, "identity.credential.register", "identity_credential", c.ID, map[string]any{"user_verification": "required"})
	return c, err
}

func (s *Store) UpdateIdentityCredentialTx(ctx context.Context, tx pgx.Tx, userID, rpID string, credentialID, data []byte) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `UPDATE identity_credentials SET credential_data=$4::jsonb,last_used_at=clock_timestamp()
		WHERE user_id=$1 AND rp_id=$2 AND credential_id=$3 AND revoked_at IS NULL RETURNING id::text`, userID, rpID, credentialID, data).Scan(&id)
	return id, err
}

func (s *Store) SaveIdentityProofTx(ctx context.Context, tx pgx.Tx, userID, sessionID, credentialID, nonceHash string) (IdentityProof, error) {
	var proof IdentityProof
	err := tx.QueryRow(ctx, `INSERT INTO identity_proofs(user_id,session_id,credential_id,nonce_hash)
		VALUES($1,$2,$3,$4) RETURNING id::text,expires_at`, userID, sessionID, credentialID, nonceHash).Scan(&proof.ID, &proof.ExpiresAt)
	if err != nil {
		return IdentityProof{}, err
	}
	err = auditMutation(ctx, tx, userID, "identity.attendance.confirm", "attendance_session", sessionID, map[string]any{"user_verification": "required"})
	return proof, err
}

// ConsumeIdentityProofTx must run in the attendance submission transaction,
// after locking the current student/session/membership and the submission nonce.
// The caller passes the same nonce digest used for submission, never a browser
// claim of biometric success. Revoking a credential invalidates unused proofs.
func (s *Store) ConsumeIdentityProofTx(ctx context.Context, tx pgx.Tx, studentID, sessionID, nonceHash, proofID string) error {
	return s.ConsumeIdentityProofForRPTx(ctx, tx, studentID, sessionID, nonceHash, proofID, "")
}

func (s *Store) ConsumeIdentityProofForRPTx(ctx context.Context, tx pgx.Tx, studentID, sessionID, nonceHash, proofID, rpID string) error {
	if _, err := parameterUUID(proofID); err != nil {
		return ErrIdentityRejected
	}
	result, err := tx.Exec(ctx, `UPDATE identity_proofs p SET used_at=clock_timestamp()
		FROM identity_credentials c WHERE p.id=$1 AND p.user_id=$2 AND p.session_id=$3 AND p.nonce_hash=$4
		AND p.used_at IS NULL AND p.expires_at>clock_timestamp() AND c.id=p.credential_id AND c.user_id=p.user_id AND c.revoked_at IS NULL
		AND ($5='' OR c.rp_id=$5)`, proofID, studentID, sessionID, nonceHash, rpID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrIdentityRejected
	}
	return nil
}

func (s *Store) RevokeIdentityCredential(ctx context.Context, actor User, rpID, credentialID string) error {
	if _, err := parameterUUID(credentialID); err != nil {
		return pgx.ErrNoRows
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := s.LockIdentityActorTx(ctx, tx, actor, ""); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE identity_credentials SET revoked_at=clock_timestamp() WHERE id=$1 AND user_id=$2 AND rp_id=$3 AND revoked_at IS NULL`, credentialID, actor.ID, rpID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	if err := auditMutation(ctx, tx, actor.ID, "identity.credential.revoke", "identity_credential", credentialID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResetStudentIdentity is an audited administrator recovery path for a lost
// phone. It invalidates all active credentials and outstanding ceremonies.
func (s *Store) ResetStudentIdentity(ctx context.Context, actor User, studentID, rpID, reason string) error {
	if len(reason) == 0 || len(reason) > 500 {
		return ErrAttendanceInvalid
	}
	if _, err := parameterUUID(studentID); err != nil {
		return pgx.ErrNoRows
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return err
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 AND role='student' FOR UPDATE`, studentID).Scan(&found); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_credentials SET revoked_at=clock_timestamp() WHERE user_id=$1 AND rp_id=$2 AND revoked_at IS NULL`, studentID, rpID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_ceremonies SET used_at=clock_timestamp() WHERE user_id=$1 AND rp_id=$2 AND used_at IS NULL`, studentID, rpID); err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "identity.student.reset", "user", studentID, map[string]any{"reason": reason}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
