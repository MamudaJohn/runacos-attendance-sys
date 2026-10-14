package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cabv/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type verificationFixture struct {
	s                               *Store
	admin, lecturer, student, other User
	course                          Course
	sessionID                       string
}

func newVerificationFixture(t *testing.T) verificationFixture {
	t.Helper()
	s := testStore(t)
	admin := addUser(t, s, "admin@attendance.test", "admin")
	lecturer := addUser(t, s, "lecturer@attendance.test", "lecturer")
	student := addUser(t, s, "student@attendance.test", "student")
	other := addUser(t, s, "other@attendance.test", "student")
	course := addCourse(t, s, "ATT101")
	mustExec(t, s, `INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)`, course.ID, lecturer.ID)
	mustExec(t, s, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2),($1,$3)`, course.ID, student.ID, other.ID)
	var roomID, sessionID string
	if err := s.Pool.QueryRow(context.Background(), `INSERT INTO rooms(name,kind,capacity) VALUES('Attendance test','room',50) RETURNING id::text`).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(context.Background(), `INSERT INTO attendance_sessions(course_id,room_id,opened_by,title,status,duration_minutes,starts_at,ends_at)
		VALUES($1,$2,$3,'Verification test','open',30,clock_timestamp()-interval '1 minute',clock_timestamp()+interval '30 minutes') RETURNING id::text`, course.ID, roomID, lecturer.ID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	return verificationFixture{s: s, admin: admin, lecturer: lecturer, student: student, other: other, course: course, sessionID: sessionID}
}

func (f verificationFixture) challenge(t *testing.T) AttendanceChallenge {
	t.Helper()
	c, err := f.s.IssueAttendanceChallenge(context.Background(), f.lecturer, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Token) != 43 || c.Generation < 1 {
		t.Fatalf("invalid challenge=%v", c)
	}
	return c
}

func (f verificationFixture) nonce(t *testing.T, student User) AttendanceNonce {
	t.Helper()
	n, err := f.s.Nonce(context.Background(), student, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Nonce) != 43 {
		t.Fatalf("invalid nonce=%v", n)
	}
	return n
}

func (f verificationFixture) verify(t *testing.T, student User, token, nonce, status, reason string, duplicate bool) {
	t.Helper()
	d, err := f.s.VerifyAttendance(context.Background(), student, f.sessionID, token, nonce)
	if err != nil || d.Status != status || d.ReasonCode != reason || d.Duplicate != duplicate {
		t.Fatalf("decision=%v error=%v want=%s/%s duplicate=%t", d, err, status, reason, duplicate)
	}
}

func TestAttendanceVerificationLifecycle(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	f.verify(t, f.student, c.Token, n.Nonce, "pending", "pending_review", false)
	f.verify(t, f.student, c.Token, n.Nonce, "rejected", "nonce_used", false)
	n = f.nonce(t, f.student)
	f.verify(t, f.student, c.Token, n.Nonce, "pending", "pending_review", true)
	var count int
	if err := f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE session_id=$1 AND student_id=$2`, f.sessionID, f.student.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("record count=%d error=%v", count, err)
	}

	// A later review is authoritative even if the student submits again.
	mustExec(t, f.s, `UPDATE attendance_records SET status='present',reason_code='manual_review',source='manual',reviewed_by=$3 WHERE session_id=$1 AND student_id=$2`, f.sessionID, f.student.ID, f.lecturer.ID)
	n = f.nonce(t, f.student)
	f.verify(t, f.student, c.Token, n.Nonce, "present", "manual_review", true)
	n = f.nonce(t, f.student)
	f.verify(t, f.student, "wrong token", n.Nonce, "rejected", "challenge_invalid", false)
	var status, source string
	if err := f.s.Pool.QueryRow(ctx, `SELECT status,source FROM attendance_records WHERE session_id=$1 AND student_id=$2`, f.sessionID, f.student.ID).Scan(&status, &source); err != nil || status != "present" || source != "manual" {
		t.Fatalf("manual review overwritten status=%s source=%s err=%v", status, source, err)
	}

	// Store neither bearer material in audits nor plaintext in credential tables.
	var challengeHash, nonceHash string
	if err := f.s.Pool.QueryRow(ctx, `SELECT token_hash FROM acoustic_challenges WHERE session_id=$1`, f.sessionID).Scan(&challengeHash); err != nil || challengeHash != auth.HashToken(c.Token) {
		t.Fatalf("challenge not hashed: %v", err)
	}
	if err := f.s.Pool.QueryRow(ctx, `SELECT nonce_hash FROM submission_nonces WHERE nonce_hash=$1`, auth.HashToken(n.Nonce)).Scan(&nonceHash); err != nil || nonceHash == n.Nonce {
		t.Fatalf("nonce not hashed: %v", err)
	}
	rows, err := f.s.Pool.Query(ctx, `SELECT metadata::text FROM audit_events WHERE target_id=$1`, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var metadata string
		if err := rows.Scan(&metadata); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{c.Token, n.Nonce, challengeHash, nonceHash} {
			if strings.Contains(metadata, secret) {
				t.Fatal("audit leaks challenge/nonce plaintext or hash")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestAttendanceVerificationFreshnessAndBinding(t *testing.T) {
	f := newVerificationFixture(t)
	c := f.challenge(t)
	oldNonce := f.nonce(t, f.student)
	n := f.nonce(t, f.student)
	f.verify(t, f.student, c.Token, oldNonce.Nonce, "rejected", "nonce_used", false)

	// Wrong owner does not consume the original owner's nonce.
	f.verify(t, f.other, c.Token, n.Nonce, "rejected", "nonce_invalid", false)
	f.verify(t, f.student, "wrong token", n.Nonce, "rejected", "challenge_invalid", false)
	f.verify(t, f.student, c.Token, n.Nonce, "rejected", "nonce_used", false)

	n = f.nonce(t, f.student)
	mustExec(t, f.s, `UPDATE submission_nonces SET expires_at=clock_timestamp()-interval '1 second' WHERE nonce_hash=$1`, auth.HashToken(n.Nonce))
	f.verify(t, f.student, c.Token, n.Nonce, "rejected", "nonce_expired", false)
	newChallenge := f.challenge(t)
	n = f.nonce(t, f.student)
	f.verify(t, f.student, c.Token, n.Nonce, "rejected", "challenge_revoked", false)
	n = f.nonce(t, f.student)
	mustExec(t, f.s, `UPDATE acoustic_challenges SET expires_at=clock_timestamp()-interval '1 second' WHERE token_hash=$1`, auth.HashToken(newChallenge.Token))
	f.verify(t, f.student, newChallenge.Token, n.Nonce, "rejected", "challenge_expired", false)

	var count int
	if err := f.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM attendance_records WHERE session_id=$1`, f.sessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejection creates attendance record count=%d err=%v", count, err)
	}
	// An initial rejection must not prevent a later valid claim.
	newChallenge = f.challenge(t)
	n = f.nonce(t, f.student)
	f.verify(t, f.student, newChallenge.Token, n.Nonce, "pending", "pending_review", false)
}

func TestAttendanceVerificationPermissionsAndWindow(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	for _, actor := range []User{f.student, {ID: f.student.ID, Role: "admin"}, f.other} {
		if _, err := f.s.IssueAttendanceChallenge(ctx, actor, f.sessionID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("nonmanager issued challenge: %v", err)
		}
	}
	if _, err := f.s.Nonce(ctx, f.lecturer, f.sessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lecturer got student nonce: %v", err)
	}
	if _, err := f.s.VerifyAttendance(ctx, f.lecturer, f.sessionID, c.Token, n.Nonce); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lecturer submits student claim: %v", err)
	}
	for _, id := range []string{"bad-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := f.s.VerifyAttendance(ctx, f.student, id, c.Token, n.Nonce); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("missing/invalid session error=%v", err)
		}
	}
	mustExec(t, f.s, `UPDATE enrollments SET status='withdrawn' WHERE course_id=$1 AND student_id=$2`, f.course.ID, f.student.ID)
	if _, err := f.s.Nonce(ctx, f.student, f.sessionID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn student got nonce: %v", err)
	}
	if _, err := f.s.VerifyAttendance(ctx, f.student, f.sessionID, c.Token, n.Nonce); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn student submitted: %v", err)
	}
	mustExec(t, f.s, `UPDATE enrollments SET status='active' WHERE course_id=$1 AND student_id=$2`, f.course.ID, f.student.ID)
	mustExec(t, f.s, `UPDATE users SET active=false WHERE id=$1`, f.student.ID)
	if _, err := f.s.VerifyAttendance(ctx, f.student, f.sessionID, c.Token, n.Nonce); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive student submitted: %v", err)
	}
	mustExec(t, f.s, `UPDATE users SET active=true WHERE id=$1`, f.student.ID)
	for _, state := range []string{"draft", "closed"} {
		mustExec(t, f.s, `UPDATE attendance_sessions SET status=$2,starts_at=NULL,ends_at=NULL WHERE id=$1`, f.sessionID, state)
		if _, err := f.s.Nonce(ctx, f.student, f.sessionID); !errors.Is(err, ErrAttendanceState) {
			t.Fatalf("%s nonce error=%v", state, err)
		}
		if _, err := f.s.IssueAttendanceChallenge(ctx, f.admin, f.sessionID); !errors.Is(err, ErrAttendanceState) {
			t.Fatalf("%s challenge error=%v", state, err)
		}
		f.verify(t, f.student, c.Token, n.Nonce, "rejected", "session_closed", false)
	}
	mustExec(t, f.s, `UPDATE attendance_sessions SET status='open',starts_at=clock_timestamp()-interval '2 minutes',ends_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, f.sessionID)
	f.verify(t, f.student, c.Token, n.Nonce, "rejected", "session_expired", false)
}

func TestAttendanceCredentialTTLAndLimits(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	mustExec(t, f.s, `UPDATE attendance_sessions SET ends_at=clock_timestamp()+interval '5 seconds' WHERE id=$1`, f.sessionID)
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	var end time.Time
	if err := f.s.Pool.QueryRow(ctx, `SELECT ends_at FROM attendance_sessions WHERE id=$1`, f.sessionID).Scan(&end); err != nil {
		t.Fatal(err)
	}
	if !c.ExpiresAt.Equal(end) || !n.ExpiresAt.Equal(end) {
		t.Fatalf("credential exceeds session end: challenge=%s nonce=%s end=%s", c.ExpiresAt, n.ExpiresAt, end)
	}
	mustExec(t, f.s, `UPDATE attendance_sessions SET ends_at=clock_timestamp()+interval '30 minutes' WHERE id=$1`, f.sessionID)
	for i := 1; i < attendanceAttemptLimit; i++ {
		f.verify(t, f.student, c.Token, "missing nonce", "rejected", "nonce_invalid", false)
	}
	f.verify(t, f.student, c.Token, "missing nonce", "rejected", "nonce_invalid", false)
	if _, err := f.s.VerifyAttendance(ctx, f.student, f.sessionID, c.Token, n.Nonce); !errors.Is(err, ErrAttendanceLimit) {
		t.Fatalf("verification cap not enforced: %v", err)
	}
	var used pgtype.Timestamptz
	if err := f.s.Pool.QueryRow(ctx, `SELECT used_at FROM submission_nonces WHERE nonce_hash=$1`, auth.HashToken(n.Nonce)).Scan(&used); err != nil || used.Valid {
		t.Fatalf("limited verification consumed nonce: %v", err)
	}
	for i := 1; i < attendanceNonceLimit; i++ {
		f.nonce(t, f.student)
	}
	if _, err := f.s.Nonce(ctx, f.student, f.sessionID); !errors.Is(err, ErrAttendanceLimit) {
		t.Fatalf("nonce cap not enforced: %v", err)
	}
	// A persisted generation cap is checked under the session lock.
	mustExec(t, f.s, `UPDATE acoustic_challenges SET generation=$2 WHERE session_id=$1`, f.sessionID, attendanceChallengeLimit)
	if _, err := f.s.IssueAttendanceChallenge(ctx, f.lecturer, f.sessionID); !errors.Is(err, ErrAttendanceLimit) {
		t.Fatalf("challenge cap not enforced: %v", err)
	}
}

func TestAttendanceVerificationConcurrency(t *testing.T) {
	f := newVerificationFixture(t)
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	var wg sync.WaitGroup
	decisions := make(chan AttendanceDecision, 2)
	errorsCh := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := f.s.VerifyAttendance(context.Background(), f.student, f.sessionID, c.Token, n.Nonce)
			decisions <- d
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(decisions)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	for d := range decisions {
		counts[d.ReasonCode]++
	}
	if counts["pending_review"] != 1 || counts["nonce_used"] != 1 {
		t.Fatalf("parallel outcomes=%v", counts)
	}
	var records int
	if err := f.s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM attendance_records WHERE session_id=$1`, f.sessionID).Scan(&records); err != nil || records != 1 {
		t.Fatalf("parallel records=%d err=%v", records, err)
	}
}

func TestAttendanceVerificationAtomicAuditFailure(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	mustExec(t, f.s, `ALTER TABLE audit_events ADD CONSTRAINT reject_verification CHECK(action <> 'attendance.verify')`)
	if _, err := f.s.VerifyAttendance(ctx, f.student, f.sessionID, c.Token, n.Nonce); postgresCode(err) != "23514" {
		t.Fatalf("audit failure error=%v", err)
	}
	var used pgtype.Timestamptz
	if err := f.s.Pool.QueryRow(ctx, `SELECT used_at FROM submission_nonces WHERE nonce_hash=$1`, auth.HashToken(n.Nonce)).Scan(&used); err != nil || used.Valid {
		t.Fatalf("audit failure consumed nonce: %v", err)
	}
	for _, table := range []string{"verification_attempts", "attendance_records"} {
		var count int
		if err := f.s.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE session_id=$1", table), f.sessionID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("audit failure persists %s count=%d err=%v", table, count, err)
		}
	}
	mustExec(t, f.s, `ALTER TABLE audit_events DROP CONSTRAINT reject_verification`)
	f.verify(t, f.student, c.Token, n.Nonce, "pending", "pending_review", false)
}

func TestAttendanceCredentialAtomicAuditFailure(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	mustExec(t, f.s, `ALTER TABLE audit_events ADD CONSTRAINT reject_new_audit CHECK(false) NOT VALID`)
	if _, err := f.s.IssueAttendanceChallenge(ctx, f.lecturer, f.sessionID); postgresCode(err) != "23514" {
		t.Fatalf("failed challenge audit error=%v", err)
	}
	if _, err := f.s.Nonce(ctx, f.student, f.sessionID); postgresCode(err) != "23514" {
		t.Fatalf("failed nonce audit error=%v", err)
	}
	var revoked, used pgtype.Timestamptz
	if err := f.s.Pool.QueryRow(ctx, `SELECT revoked_at FROM acoustic_challenges WHERE token_hash=$1`, auth.HashToken(c.Token)).Scan(&revoked); err != nil || revoked.Valid {
		t.Fatalf("failed challenge issue revoked prior credential: %v", err)
	}
	if err := f.s.Pool.QueryRow(ctx, `SELECT used_at FROM submission_nonces WHERE nonce_hash=$1`, auth.HashToken(n.Nonce)).Scan(&used); err != nil || used.Valid {
		t.Fatalf("failed nonce issue retired prior credential: %v", err)
	}
	for _, table := range []string{"acoustic_challenges", "submission_nonces"} {
		var count int
		if err := f.s.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE session_id=$1", table), f.sessionID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("failed audit inserted extra %s count=%d err=%v", table, count, err)
		}
	}
	mustExec(t, f.s, `ALTER TABLE audit_events DROP CONSTRAINT reject_new_audit`)
	f.verify(t, f.student, c.Token, n.Nonce, "pending", "pending_review", false)
}

func TestAttendanceVerificationExpiryAfterLockWait(t *testing.T) {
	f := newVerificationFixture(t)
	ctx := context.Background()
	c := f.challenge(t)
	n := f.nonce(t, f.student)
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM attendance_sessions WHERE id=$1 FOR UPDATE`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	result := make(chan AttendanceDecision, 1)
	errorsCh := make(chan error, 1)
	go func() {
		d, err := f.s.VerifyAttendance(ctx, f.student, f.sessionID, c.Token, n.Nonce)
		result <- d
		errorsCh <- err
	}()
	select {
	case d := <-result:
		t.Fatalf("verification escaped session lock: %v", d)
	case <-time.After(250 * time.Millisecond):
	}
	// The deadline passes after verification's transaction began, while it was
	// blocked. Transaction-start now() could incorrectly accept this claim.
	if _, err := tx.Exec(ctx, `UPDATE attendance_sessions SET ends_at=clock_timestamp()-interval '25 milliseconds' WHERE id=$1`, f.sessionID); err != nil {
		t.Fatal(err)
	}
	var releasedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-result:
		if err := <-errorsCh; err != nil || d.Status != "rejected" || d.ReasonCode != "session_expired" {
			t.Fatalf("expired after wait decision=%v error=%v", d, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verification remained blocked after session unlock")
	}
	var attemptedAt time.Time
	if err := f.s.Pool.QueryRow(ctx, `SELECT created_at FROM verification_attempts WHERE session_id=$1 AND student_id=$2`, f.sessionID, f.student.ID).Scan(&attemptedAt); err != nil || attemptedAt.Before(releasedAt) {
		t.Fatalf("attempt chronology predates lock release: attempted=%s released=%s error=%v", attemptedAt, releasedAt, err)
	}
}
