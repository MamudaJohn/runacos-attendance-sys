package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type attendanceTestFixture struct {
	store                              *Store
	admin, lecturer, student, stranger User
	course                             Course
	room                               Room
	session                            AttendanceSession
}

func attendanceFixture(t *testing.T) attendanceTestFixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	f := attendanceTestFixture{store: s, admin: addUser(t, s, "admin@test.example", "admin"),
		lecturer: addUser(t, s, "lecturer@test.example", "lecturer"), student: addUser(t, s, "student@test.example", "student"),
		stranger: addUser(t, s, "stranger@test.example", "student"), course: addCourse(t, s, "CSC101")}
	mustExec(t, s, `INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)`, f.course.ID, f.lecturer.ID)
	mustExec(t, s, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)`, f.course.ID, f.student.ID)
	var err error
	f.room, err = s.SaveRoom(ctx, f.admin, "", "Room 1", "room", 40)
	if err != nil {
		t.Fatal(err)
	}
	f.session, err = s.CreateAttendanceSession(ctx, f.lecturer, f.course.ID, f.room.ID, " Lecture one ", 10)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAttendanceLifecycleAndExpiry(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	if f.session.Status != "draft" || f.session.Title != "Lecture one" || !f.session.StartsAt.IsZero() || !f.session.EndsAt.IsZero() || f.session.PolicyVersion != AttendancePolicyVersion {
		t.Fatalf("draft=%+v", f.session)
	}
	for _, user := range []User{f.admin, f.lecturer, f.student} {
		sessions, err := s.ListAttendanceSessions(ctx, user, f.course.ID)
		if err != nil || len(sessions) != 1 || sessions[0] != f.session {
			t.Fatalf("session listing role=%s sessions=%v error=%v", user.Role, sessions, err)
		}
	}
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	open, err := s.AttendanceSession(ctx, f.student, f.session.ID)
	if err != nil || open.Status != "open" || open.StartsAt.IsZero() || open.EndsAt.Sub(open.StartsAt) != 10*time.Minute {
		t.Fatalf("open=%+v error=%v", open, err)
	}
	if err := s.OpenAttendanceSession(ctx, f.admin, f.session.ID); !errors.Is(err, ErrAttendanceState) {
		t.Fatalf("double open error=%v", err)
	}
	mustExec(t, s, `UPDATE attendance_sessions SET starts_at=clock_timestamp()-interval '11 minutes',ends_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, f.session.ID)
	expired, err := s.AttendanceSession(ctx, f.student, f.session.ID)
	if err != nil || expired.Status != "expired" {
		t.Fatalf("expired=%+v error=%v", expired, err)
	}
	sessions, err := s.ListAttendanceSessions(ctx, f.lecturer, f.course.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Status != "expired" {
		t.Fatalf("expired list=%+v error=%v", sessions, err)
	}
	if err := s.CloseAttendanceSession(ctx, f.admin, f.session.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatalf("repeat close: %v", err)
	}
	if err := s.OpenAttendanceSession(ctx, f.admin, f.session.ID); !errors.Is(err, ErrAttendanceState) {
		t.Fatalf("reopen error=%v", err)
	}
	assertAuditCount(t, s, 4) // room, draft, open, close; repeated close is idempotent.
	closedDraft, err := s.CreateAttendanceSession(ctx, f.admin, f.course.ID, f.room.ID, "Cancelled", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CloseAttendanceSession(ctx, f.lecturer, closedDraft.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReviewAttendance(ctx, f.lecturer, closedDraft.ID, f.student.Email, "present", "Manual fallback"); !errors.Is(err, ErrAttendanceState) {
		t.Fatalf("closed unopened draft reviewed: %v", err)
	}
}

func TestAttendanceCloseRevokesChallengesAndNonces(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acoustic_challenges(session_id,token_hash,generation,expires_at)
		VALUES($1,$2,1,clock_timestamp()+interval '1 minute')`, f.session.ID, strings.Repeat("a", 64))
	mustExec(t, s, `INSERT INTO submission_nonces(session_id,student_id,nonce_hash,expires_at)
		VALUES($1,$2,$3,clock_timestamp()+interval '1 minute')`, f.session.ID, f.student.ID, strings.Repeat("b", 64))
	if err := s.CloseAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	var revoked, used int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM acoustic_challenges WHERE session_id=$1 AND revoked_at IS NOT NULL`, f.session.ID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("revoked=%d error=%v", revoked, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM submission_nonces WHERE session_id=$1 AND used_at IS NOT NULL`, f.session.ID).Scan(&used); err != nil || used != 1 {
		t.Fatalf("used=%d error=%v", used, err)
	}
}

func TestAttendanceAuthorityRechecksDatabaseState(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	assertManagerDenied := func(actor User) {
		t.Helper()
		if _, err := s.CreateAttendanceSession(ctx, actor, f.course.ID, f.room.ID, "Denied", 1); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("create role=%s error=%v", actor.Role, err)
		}
		if err := s.OpenAttendanceSession(ctx, actor, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("open role=%s error=%v", actor.Role, err)
		}
		if err := s.CloseAttendanceSession(ctx, actor, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("close role=%s error=%v", actor.Role, err)
		}
		if _, err := s.AttendanceResults(ctx, actor, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("roster role=%s error=%v", actor.Role, err)
		}
		if _, err := s.AttendanceRooms(ctx, actor, f.course.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("rooms role=%s error=%v", actor.Role, err)
		}
		if err := s.ReviewAttendance(ctx, actor, f.session.ID, f.student.Email, "present", "Manual"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("review role=%s error=%v", actor.Role, err)
		}
	}
	for _, actor := range []User{f.student, f.stranger, {ID: f.student.ID, Role: "admin"}, {ID: "bad", Role: "admin"}} {
		assertManagerDenied(actor)
	}
	rooms, err := s.AttendanceRooms(ctx, f.lecturer, f.course.ID)
	if err != nil || len(rooms) != 1 || rooms[0] != f.room {
		t.Fatalf("lecturer rooms=%v error=%v", rooms, err)
	}
	mustExec(t, s, `DELETE FROM course_lecturers WHERE course_id=$1 AND lecturer_id=$2`, f.course.ID, f.lecturer.ID)
	assertManagerDenied(f.lecturer)
	mustExec(t, s, `UPDATE users SET active=false WHERE id=$1`, f.admin.ID)
	assertManagerDenied(f.admin)
	for _, user := range []User{f.stranger, f.lecturer, f.admin} {
		if _, err := s.AttendanceSession(ctx, user, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("revoked/unrelated detail role=%s error=%v", user.Role, err)
		}
	}
	mustExec(t, s, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, f.student.ID)
	if _, err := s.MyAttendance(ctx, f.student, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn own attendance error=%v", err)
	}
	if _, err := s.AttendanceSession(ctx, f.student, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn attendance detail error=%v", err)
	}
	assertAuditCount(t, s, 2)
}

func TestAttendanceManualReviewPreservesEvidenceAndHistory(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	if err := s.ReviewAttendance(ctx, f.lecturer, f.session.ID, f.student.Email, "present", "Manual fallback"); !errors.Is(err, ErrAttendanceState) {
		t.Fatalf("draft review error=%v", err)
	}
	results, err := s.AttendanceResults(ctx, f.lecturer, f.session.ID)
	if err != nil || len(results) != 1 || results[0].StudentID != f.student.ID || results[0].Status != "not_submitted" {
		t.Fatalf("unsubmitted results=%+v error=%v", results, err)
	}
	if _, err := s.MyAttendance(ctx, f.admin, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("admin student result error=%v", err)
	}
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	var attemptID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO verification_attempts(session_id,student_id,decision,reason_code,policy_version)
		VALUES($1,$2,'pending','pending_review',$3) RETURNING id::text`, f.session.ID, f.student.ID, AttendancePolicyVersion).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO attendance_records(session_id,student_id,status,source,reason_code,source_attempt_id)
		VALUES($1,$2,'pending','challenge','pending_review',$3)`, f.session.ID, f.student.ID, attemptID)
	if err := s.CloseAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"present", "rejected", "present"} {
		if err := s.ReviewAttendance(ctx, f.lecturer, f.session.ID, " STUDENT@Test.Example ", decision, " Confirmed by roster review "); err != nil {
			t.Fatal(err)
		}
		own, err := s.MyAttendance(ctx, f.student, f.session.ID)
		if err != nil || own.Status != decision || own.Source != "manual" || own.ReasonCode != "manual_review" || own.ReviewedBy != f.lecturer.ID || own.ReviewReason != "Confirmed by roster review" || !own.CanReview {
			t.Fatalf("review decision=%s own=%+v error=%v", decision, own, err)
		}
	}
	var count, reviews int
	var preservedAttempt, verdict string
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE session_id=$1 AND student_id=$2`, f.session.ID, f.student.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate records=%d error=%v", count, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT source_attempt_id::text FROM attendance_records WHERE session_id=$1 AND student_id=$2`, f.session.ID, f.student.ID).Scan(&preservedAttempt); err != nil || preservedAttempt != attemptID {
		t.Fatalf("preserved attempt=%q error=%v", preservedAttempt, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT decision FROM verification_attempts WHERE id=$1`, attemptID).Scan(&verdict); err != nil || verdict != "pending" {
		t.Fatalf("original verdict=%q error=%v", verdict, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_reviews WHERE session_id=$1 AND student_id=$2`, f.session.ID, f.student.ID).Scan(&reviews); err != nil || reviews != 3 {
		t.Fatalf("reviews=%d error=%v", reviews, err)
	}
	if err := s.ReviewAttendance(ctx, f.admin, f.session.ID, f.stranger.Email, "present", "Not enrolled"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unrelated student review error=%v", err)
	}
	assertAuditCount(t, s, 7)
	mustExec(t, s, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, f.student.ID)
	results, err = s.AttendanceResults(ctx, f.lecturer, f.session.ID)
	if err != nil || len(results) != 1 || results[0].StudentID != f.student.ID || results[0].Status != "present" || results[0].CanReview {
		t.Fatalf("withdrawal hid history or permits review results=%+v error=%v", results, err)
	}
	if err := s.ReviewAttendance(ctx, f.admin, f.session.ID, f.student.Email, "rejected", "After withdrawal"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn review error=%v", err)
	}
	mustExec(t, s, `UPDATE enrollments SET status='active' WHERE student_id=$1`, f.student.ID)
	mustExec(t, s, `UPDATE users SET active=false WHERE id=$1`, f.student.ID)
	results, err = s.AttendanceResults(ctx, f.admin, f.session.ID)
	if err != nil || len(results) != 1 || results[0].Status != "present" || results[0].CanReview {
		t.Fatalf("disabled account hid history or permits review results=%+v error=%v", results, err)
	}
	if err := s.ReviewAttendance(ctx, f.admin, f.session.ID, f.student.Email, "rejected", "After disabling"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled review error=%v", err)
	}
}

func TestAttendanceRejectedAttemptRemainsVisibleWithoutARecord(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	mustExec(t, s, `INSERT INTO verification_attempts(session_id,student_id,decision,reason_code,policy_version)
		VALUES($1,$2,'rejected','challenge_expired',$3)`, f.session.ID, f.student.ID, AttendancePolicyVersion)
	own, err := s.MyAttendance(ctx, f.student, f.session.ID)
	if err != nil || own.Status != "rejected" || own.Source != "challenge" || own.ReasonCode != "challenge_expired" || own.ReviewReason != "" {
		t.Fatalf("rejected own=%+v error=%v", own, err)
	}
}

func TestAttendanceValidationAndSchemaOwnership(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	for _, tc := range []struct {
		title    string
		duration int
	}{{"", 1}, {" ", 1}, {strings.Repeat("x", 201), 1}, {"Invalid", 0}, {"Invalid", 181}} {
		if _, err := s.CreateAttendanceSession(ctx, f.lecturer, f.course.ID, f.room.ID, tc.title, tc.duration); !errors.Is(err, ErrAttendanceInvalid) {
			t.Fatalf("title=%q duration=%d error=%v", tc.title, tc.duration, err)
		}
	}
	for _, tc := range []struct{ decision, reason string }{{"pending", "Reason"}, {"present", " "}, {"present", strings.Repeat("r", 501)}} {
		if err := s.ReviewAttendance(ctx, f.lecturer, f.session.ID, f.student.Email, tc.decision, tc.reason); !errors.Is(err, ErrAttendanceInvalid) {
			t.Fatalf("decision=%q reason=%q error=%v", tc.decision, tc.reason, err)
		}
	}
	for _, id := range []string{"bad", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.AttendanceSession(ctx, f.student, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid session ID=%s error=%v", id, err)
		}
		if _, err := s.CreateAttendanceSession(ctx, f.admin, f.course.ID, id, "Missing room", 1); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid room ID=%s error=%v", id, err)
		}
	}
	for _, query := range []string{
		`UPDATE attendance_sessions SET status='open',starts_at=now(),ends_at=NULL WHERE id=$1`,
		`UPDATE attendance_sessions SET status='draft',starts_at=now(),ends_at=now()+interval '1 minute' WHERE id=$1`,
		`UPDATE attendance_sessions SET status='open',starts_at=NULL,ends_at=now()+interval '1 minute' WHERE id=$1`,
	} {
		if _, err := s.Pool.Exec(ctx, query, f.session.ID); postgresCode(err) != "23514" {
			t.Fatalf("invalid session times accepted error=%v", err)
		}
	}
	var attemptID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO verification_attempts(session_id,student_id,decision,reason_code,policy_version)
		VALUES($1,$2,'pending','pending_review',$3) RETURNING id::text`, f.session.ID, f.student.ID, AttendancePolicyVersion).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO attendance_records(session_id,student_id,status,source,reason_code,source_attempt_id)
		VALUES($1,$2,'pending','challenge','pending_review',$3)`, f.session.ID, f.stranger.ID, attemptID); postgresCode(err) != "23503" {
		t.Fatalf("cross student attempt accepted error=%v", err)
	}
}

func TestAttendanceAuditFailureRollsBackEveryMutation(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	mustExec(t, s, `ALTER TABLE audit_events ADD CONSTRAINT reject_future CHECK(false) NOT VALID`)
	if _, err := s.CreateAttendanceSession(ctx, f.admin, f.course.ID, f.room.ID, "Failed create", 1); postgresCode(err) != "23514" {
		t.Fatalf("create audit failure=%v", err)
	}
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); postgresCode(err) != "23514" {
		t.Fatalf("open audit failure=%v", err)
	}
	session, err := s.AttendanceSession(ctx, f.student, f.session.ID)
	if err != nil || session.Status != "draft" || !session.StartsAt.IsZero() {
		t.Fatalf("failed open mutated session=%+v error=%v", session, err)
	}
	mustExec(t, s, `ALTER TABLE audit_events DROP CONSTRAINT reject_future`)
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO acoustic_challenges(session_id,token_hash,generation,expires_at)
		VALUES($1,$2,1,clock_timestamp()+interval '1 minute')`, f.session.ID, strings.Repeat("a", 64))
	mustExec(t, s, `INSERT INTO submission_nonces(session_id,student_id,nonce_hash,expires_at)
		VALUES($1,$2,$3,clock_timestamp()+interval '1 minute')`, f.session.ID, f.student.ID, strings.Repeat("b", 64))
	mustExec(t, s, `ALTER TABLE audit_events ADD CONSTRAINT reject_future CHECK(false) NOT VALID`)
	if err := s.CloseAttendanceSession(ctx, f.lecturer, f.session.ID); postgresCode(err) != "23514" {
		t.Fatalf("close audit failure=%v", err)
	}
	if err := s.ReviewAttendance(ctx, f.lecturer, f.session.ID, f.student.Email, "present", "Checked manual fallback"); postgresCode(err) != "23514" {
		t.Fatalf("review audit failure=%v", err)
	}
	var count int
	for _, table := range []string{"attendance_records", "attendance_reviews"} {
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed mutation table=%s count=%d error=%v", table, count, err)
		}
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM acoustic_challenges WHERE revoked_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed close revoked challenge count=%d error=%v", count, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM submission_nonces WHERE used_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed close used nonce count=%d error=%v", count, err)
	}
	session, err = s.AttendanceSession(ctx, f.student, f.session.ID)
	if err != nil || session.Status != "open" {
		t.Fatalf("failed close mutated session=%+v error=%v", session, err)
	}
	assertAuditCount(t, s, 3)
}

func TestAttendanceReviewReasonMatchesLastDecisionAfterWaitingForLock(t *testing.T) {
	f := attendanceFixture(t)
	s := f.store
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM attendance_sessions WHERE id=$1 FOR UPDATE`, f.session.ID); err != nil {
		t.Fatal(err)
	}
	// A separate single-connection pool lets the test observe the waiting request
	// exactly, without relying on a sleep or other tests' database connections.
	cfg := s.Pool.Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	workerPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer workerPool.Close()
	var workerPID int
	if err := workerPool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&workerPID); err != nil {
		t.Fatal(err)
	}
	worker := &Store{Pool: workerPool}
	done := make(chan error, 1)
	go func() {
		done <- worker.ReviewAttendance(ctx, f.lecturer, f.session.ID, f.student.Email, "rejected", "Final decision after waiting")
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE pid=$1 AND wait_event_type='Lock')`, workerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("review finished without waiting for lock: %v", err)
		case <-deadline.C:
			t.Fatal("review did not reach session lock")
		case <-ticker.C:
		}
	}
	// This later transaction writes first. The older waiting request must become
	// the latest review after it resumes, including the displayed review reason.
	if _, err := blocker.Exec(ctx, `INSERT INTO attendance_reviews(session_id,student_id,actor_id,decision,reason)
		VALUES($1,$2,$3,'present','Earlier committed decision')`, f.session.ID, f.student.ID, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `INSERT INTO attendance_records(session_id,student_id,status,source,reason_code,reviewed_by)
		VALUES($1,$2,'present','manual','manual_review',$3)`, f.session.ID, f.student.ID, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	own, err := s.MyAttendance(ctx, f.student, f.session.ID)
	if err != nil || own.Status != "rejected" || own.ReviewReason != "Final decision after waiting" || own.ReviewedBy != f.lecturer.ID {
		t.Fatalf("last decision and reason diverged own=%+v error=%v", own, err)
	}
}
