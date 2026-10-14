package store

import (
	"context"
	"errors"
	"time"

	"cabv/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	attendanceChallengeTTL   = 90 * time.Second
	attendanceNonceTTL       = 2 * time.Minute
	attendanceChallengeLimit = 1000
	attendanceNonceLimit     = 50
	attendanceAttemptLimit   = 20
)

// AttendanceChallenge is the only place a challenge's plaintext is returned.
// It is emitted once, to the assigned lecturer or administrator issuing it.
type AttendanceChallenge struct {
	Token      string
	Generation int
	ExpiresAt  time.Time
}

type AttendanceNonce struct {
	Nonce     string
	ExpiresAt time.Time
}

// A successful token submission is pending human review, not proof of presence.
type AttendanceDecision struct {
	Status     string
	ReasonCode string
	Duplicate  bool
}

func attendanceClock(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return now, err
}

func attendanceWindowReason(session AttendanceSession, now time.Time) string {
	if session.Status != "open" {
		return "session_closed"
	}
	if session.StartsAt.IsZero() || now.Before(session.StartsAt) || session.EndsAt.IsZero() || !now.Before(session.EndsAt) {
		return "session_expired"
	}
	return ""
}

func attendanceExpiry(now, end time.Time, ttl time.Duration) time.Time {
	expires := now.Add(ttl)
	if end.Before(expires) {
		expires = end
	}
	return expires
}

// IssueAttendanceChallenge rotates the existing challenge under the same row
// lock used by verification and closure. Neither the database nor audit stores
// its plaintext, so it cannot be retrieved later; the lecturer can rotate again.
func (s *Store) IssueAttendanceChallenge(ctx context.Context, actor User, sessionID string) (AttendanceChallenge, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceChallenge{}, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return AttendanceChallenge{}, err
	}
	now, err := attendanceClock(ctx, tx)
	if err != nil {
		return AttendanceChallenge{}, err
	}
	if attendanceWindowReason(session, now) != "" {
		return AttendanceChallenge{}, ErrAttendanceState
	}
	var generation int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(generation), 0) + 1 FROM acoustic_challenges WHERE session_id=$1`, session.ID).Scan(&generation); err != nil {
		return AttendanceChallenge{}, err
	}
	if generation > attendanceChallengeLimit {
		return AttendanceChallenge{}, ErrAttendanceLimit
	}
	token, err := auth.Token()
	if err != nil {
		return AttendanceChallenge{}, err
	}
	expires := attendanceExpiry(now, session.EndsAt, attendanceChallengeTTL)
	if _, err := tx.Exec(ctx, `UPDATE acoustic_challenges SET revoked_at=$2 WHERE session_id=$1 AND revoked_at IS NULL`, session.ID, now); err != nil {
		return AttendanceChallenge{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO acoustic_challenges(session_id,token_hash,generation,expires_at,created_at)
		VALUES ($1,$2,$3,$4,clock_timestamp())`, session.ID, auth.HashToken(token), generation, expires); err != nil {
		return AttendanceChallenge{}, err
	}
	if err := auditMutation(ctx, tx, actor.ID, "attendance.challenge.rotate", "attendance_session", session.ID,
		map[string]any{"generation": generation, "expires_at": expires}); err != nil {
		return AttendanceChallenge{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AttendanceChallenge{}, err
	}
	return AttendanceChallenge{Token: token, Generation: generation, ExpiresAt: expires}, nil
}

// Nonce grants one short-lived attempt, bound to the current student and session.
// Reissuing retires any previous unused nonce; the request count persists in DB.
func (s *Store) Nonce(ctx context.Context, student User, sessionID string) (AttendanceNonce, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceNonce{}, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceStudent(ctx, tx, student, sessionID)
	if err != nil {
		return AttendanceNonce{}, err
	}
	now, err := attendanceClock(ctx, tx)
	if err != nil {
		return AttendanceNonce{}, err
	}
	if attendanceWindowReason(session, now) != "" {
		return AttendanceNonce{}, ErrAttendanceState
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM submission_nonces WHERE session_id=$1 AND student_id=$2`, session.ID, student.ID).Scan(&count); err != nil {
		return AttendanceNonce{}, err
	}
	if count >= attendanceNonceLimit {
		return AttendanceNonce{}, ErrAttendanceLimit
	}
	nonce, err := auth.Token()
	if err != nil {
		return AttendanceNonce{}, err
	}
	expires := attendanceExpiry(now, session.EndsAt, attendanceNonceTTL)
	if _, err := tx.Exec(ctx, `UPDATE submission_nonces SET used_at=$3
		WHERE session_id=$1 AND student_id=$2 AND used_at IS NULL`, session.ID, student.ID, now); err != nil {
		return AttendanceNonce{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO submission_nonces(session_id,student_id,nonce_hash,expires_at,created_at)
		VALUES ($1,$2,$3,$4,clock_timestamp())`, session.ID, student.ID, auth.HashToken(nonce), expires); err != nil {
		return AttendanceNonce{}, err
	}
	if err := auditMutation(ctx, tx, student.ID, "attendance.nonce.issue", "attendance_session", session.ID, nil); err != nil {
		return AttendanceNonce{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AttendanceNonce{}, err
	}
	return AttendanceNonce{Nonce: nonce, ExpiresAt: expires}, nil
}

// VerifyAttendance atomically consumes an owned, fresh nonce, writes an attempt
// and its audit, and (only for a valid claim) creates a pending review record.
// The session lock serializes closure, rotation, limits, and duplicate handling.
// Failed attempts never replace an existing attendance or manual review.
func (s *Store) VerifyAttendance(ctx context.Context, student User, sessionID, token, nonce string) (AttendanceDecision, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceDecision{}, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceStudent(ctx, tx, student, sessionID)
	if err != nil {
		return AttendanceDecision{}, err
	}
	now, err := attendanceClock(ctx, tx)
	if err != nil {
		return AttendanceDecision{}, err
	}
	var attempts int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM verification_attempts WHERE session_id=$1 AND student_id=$2`, session.ID, student.ID).Scan(&attempts); err != nil {
		return AttendanceDecision{}, err
	}
	if attempts >= attendanceAttemptLimit {
		return AttendanceDecision{}, ErrAttendanceLimit
	}

	// Look up only the caller's nonce: a wrong-owner submission cannot consume
	// another student's attempt or reveal that the supplied nonce exists.
	var nonceID string
	var nonceExpires time.Time
	var nonceUsed pgtype.Timestamptz
	nonceErr := tx.QueryRow(ctx, `SELECT id::text,expires_at,used_at FROM submission_nonces
		WHERE session_id=$1 AND student_id=$2 AND nonce_hash=$3 FOR UPDATE`, session.ID, student.ID, auth.HashToken(nonce)).
		Scan(&nonceID, &nonceExpires, &nonceUsed)
	if nonceErr != nil && !errors.Is(nonceErr, pgx.ErrNoRows) {
		return AttendanceDecision{}, nonceErr
	}
	reason := attendanceWindowReason(session, now)
	nonceReason := ""
	switch {
	case errors.Is(nonceErr, pgx.ErrNoRows):
		nonceReason = "nonce_invalid"
	case nonceUsed.Valid:
		nonceReason = "nonce_used"
	case !now.Before(nonceExpires):
		nonceReason = "nonce_expired"
	default:
		if _, err := tx.Exec(ctx, `UPDATE submission_nonces SET used_at=$2 WHERE id=$1`, nonceID, now); err != nil {
			return AttendanceDecision{}, err
		}
	}
	if reason == "" {
		reason = nonceReason
	}

	var challengeID any
	if reason == "" {
		var foundID string
		var expires time.Time
		var revoked pgtype.Timestamptz
		var generation, currentGeneration int
		err := tx.QueryRow(ctx, `SELECT id::text,expires_at,revoked_at,generation,
			(SELECT max(generation) FROM acoustic_challenges WHERE session_id=$1)
			FROM acoustic_challenges WHERE session_id=$1 AND token_hash=$2`, session.ID, auth.HashToken(token)).
			Scan(&foundID, &expires, &revoked, &generation, &currentGeneration)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return AttendanceDecision{}, err
		}
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = "challenge_invalid"
		case revoked.Valid || generation != currentGeneration:
			challengeID = foundID
			reason = "challenge_revoked"
		case !now.Before(expires):
			challengeID = foundID
			reason = "challenge_expired"
		default:
			challengeID = foundID
		}
	}

	decision := AttendanceDecision{Status: "rejected", ReasonCode: reason}
	attemptDecision := "rejected"
	if reason == "" {
		decision = AttendanceDecision{Status: "pending", ReasonCode: "pending_review"}
		attemptDecision = "pending"
		var priorStatus, priorReason string
		err := tx.QueryRow(ctx, `SELECT status,reason_code FROM attendance_records WHERE session_id=$1 AND student_id=$2`, session.ID, student.ID).
			Scan(&priorStatus, &priorReason)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return AttendanceDecision{}, err
		}
		if err == nil {
			decision = AttendanceDecision{Status: priorStatus, ReasonCode: priorReason, Duplicate: true}
		}
	}
	attemptReason := decision.ReasonCode
	if decision.Duplicate {
		attemptReason = "duplicate"
	}
	var attemptID string
	if err := tx.QueryRow(ctx, `INSERT INTO verification_attempts(session_id,student_id,challenge_id,decision,reason_code,policy_version,created_at)
		VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()) RETURNING id::text`, session.ID, student.ID, challengeID, attemptDecision, attemptReason, session.PolicyVersion).
		Scan(&attemptID); err != nil {
		return AttendanceDecision{}, err
	}
	if attemptDecision == "pending" && !decision.Duplicate {
		if _, err := tx.Exec(ctx, `INSERT INTO attendance_records(session_id,student_id,status,reason_code,source,source_attempt_id,updated_at)
			VALUES($1,$2,'pending','pending_review','challenge',$3,clock_timestamp())`, session.ID, student.ID, attemptID); err != nil {
			return AttendanceDecision{}, err
		}
	}
	if err := auditMutation(ctx, tx, student.ID, "attendance.verify", "attendance_session", session.ID,
		map[string]any{"attempt_id": attemptID, "decision": attemptDecision, "reason_code": attemptReason, "policy_version": session.PolicyVersion}); err != nil {
		return AttendanceDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AttendanceDecision{}, err
	}
	return decision, nil
}
