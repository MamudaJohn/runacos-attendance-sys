package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

var (
	ErrAttendanceState   = errors.New("attendance session is not in the required state")
	ErrAttendanceInvalid = errors.New("invalid attendance input")
	ErrAttendanceLimit   = errors.New("attendance request limit reached")
)

const AttendancePolicyVersion = "token-review-v1"

type AttendanceSession struct {
	ID, CourseID, RoomID, RoomName, Title, Status, PolicyVersion string
	StartsAt, EndsAt                                             time.Time
	DurationMinutes                                              int
}

type AttendanceResult struct {
	StudentID, Email, Name, Status, Source, ReasonCode string
	ReviewReason, ReviewedBy, ReviewerName             string
	UpdatedAt                                          time.Time
	CanReview                                          bool
}

const attendanceSessionColumns = `a.id::text, a.course_id::text, a.room_id::text, r.name,
	a.title, a.status, a.policy_version, a.starts_at, a.ends_at, a.duration_minutes`

func scanAttendanceSession(row pgx.Row) (AttendanceSession, error) {
	var session AttendanceSession
	var start, end *time.Time
	err := row.Scan(&session.ID, &session.CourseID, &session.RoomID, &session.RoomName,
		&session.Title, &session.Status, &session.PolicyVersion, &start, &end, &session.DurationMinutes)
	if start != nil {
		session.StartsAt = *start
	}
	if end != nil {
		session.EndsAt = *end
	}
	return session, err
}

// Lock order matches catalog writes: current account, course, session, membership.
// The course lock serializes assignment/enrollment edits until this operation ends.
// Authority always comes from current database state, never the caller's role.
func lockAttendanceUser(ctx context.Context, tx pgx.Tx, actor User, student bool) (string, error) {
	id, err := parameterUUID(actor.ID)
	if err != nil {
		return "", err
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND active FOR SHARE`, id).Scan(&role); err != nil {
		return "", err
	}
	if (student && role != "student") || (!student && role != "admin" && role != "lecturer") {
		return "", pgx.ErrNoRows
	}
	return role, nil
}

func lockAttendanceMembership(ctx context.Context, tx pgx.Tx, actor User, role, courseID string) error {
	var found string
	switch role {
	case "admin":
		return nil
	case "lecturer":
		return tx.QueryRow(ctx, `SELECT course_id::text FROM course_lecturers
			WHERE course_id=$1 AND lecturer_id=$2 FOR SHARE`, courseID, actor.ID).Scan(&found)
	case "student":
		return tx.QueryRow(ctx, `SELECT course_id::text FROM enrollments
			WHERE course_id=$1 AND student_id=$2 AND status='active' FOR SHARE`, courseID, actor.ID).Scan(&found)
	default:
		return pgx.ErrNoRows
	}
}

func lockAttendanceSession(ctx context.Context, tx pgx.Tx, actor User, sessionID string, student bool) (AttendanceSession, error) {
	id, err := parameterUUID(sessionID)
	if err != nil {
		return AttendanceSession{}, err
	}
	role, err := lockAttendanceUser(ctx, tx, actor, student)
	if err != nil {
		return AttendanceSession{}, err
	}
	var courseID, found string
	if err := tx.QueryRow(ctx, `SELECT course_id::text FROM attendance_sessions WHERE id=$1`, id).Scan(&courseID); err != nil {
		return AttendanceSession{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id=$1 FOR SHARE`, courseID).Scan(&found); err != nil {
		return AttendanceSession{}, err
	}
	session, err := scanAttendanceSession(tx.QueryRow(ctx, `SELECT `+attendanceSessionColumns+`
		FROM attendance_sessions a JOIN rooms r ON r.id=a.room_id WHERE a.id=$1 FOR UPDATE OF a`, id))
	if err != nil {
		return AttendanceSession{}, err
	}
	if session.CourseID != courseID {
		return AttendanceSession{}, pgx.ErrNoRows
	}
	if err := lockAttendanceMembership(ctx, tx, actor, role, courseID); err != nil {
		return AttendanceSession{}, err
	}
	return session, nil
}

func lockAttendanceManager(ctx context.Context, tx pgx.Tx, actor User, sessionID string) (AttendanceSession, error) {
	return lockAttendanceSession(ctx, tx, actor, sessionID, false)
}

func lockAttendanceStudent(ctx context.Context, tx pgx.Tx, actor User, sessionID string) (AttendanceSession, error) {
	return lockAttendanceSession(ctx, tx, actor, sessionID, true)
}

func (s *Store) CreateAttendanceSession(ctx context.Context, actor User, courseID, roomID, title string, durationMinutes int) (AttendanceSession, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 || !utf8.ValidString(title) || durationMinutes < 1 || durationMinutes > 180 {
		return AttendanceSession{}, ErrAttendanceInvalid
	}
	course, err := parameterUUID(courseID)
	if err != nil {
		return AttendanceSession{}, err
	}
	room, err := parameterUUID(roomID)
	if err != nil {
		return AttendanceSession{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceSession{}, err
	}
	defer tx.Rollback(ctx)
	role, err := lockAttendanceUser(ctx, tx, actor, false)
	if err != nil {
		return AttendanceSession{}, err
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id=$1 FOR SHARE`, course).Scan(&found); err != nil {
		return AttendanceSession{}, err
	}
	if err := lockAttendanceMembership(ctx, tx, actor, role, courseID); err != nil {
		return AttendanceSession{}, err
	}
	var roomName string
	if err := tx.QueryRow(ctx, `SELECT name FROM rooms WHERE id=$1 FOR SHARE`, room).Scan(&roomName); err != nil {
		return AttendanceSession{}, err
	}
	session := AttendanceSession{CourseID: courseID, RoomID: roomID, RoomName: roomName,
		Title: title, Status: "draft", PolicyVersion: AttendancePolicyVersion, DurationMinutes: durationMinutes}
	if err := tx.QueryRow(ctx, `INSERT INTO attendance_sessions(course_id,room_id,opened_by,title,duration_minutes)
		VALUES($1,$2,$3,$4,$5) RETURNING id::text`, course, room, actor.ID, title, durationMinutes).Scan(&session.ID); err != nil {
		return AttendanceSession{}, err
	}
	if err := auditMutation(ctx, tx, actor.ID, "attendance.session_created", "attendance_session", session.ID,
		map[string]any{"course_id": courseID, "room_id": roomID, "duration_minutes": durationMinutes, "policy_version": AttendancePolicyVersion}); err != nil {
		return AttendanceSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AttendanceSession{}, err
	}
	return session, nil
}

func (s *Store) OpenAttendanceSession(ctx context.Context, actor User, sessionID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return err
	}
	if session.Status != "draft" {
		return ErrAttendanceState
	}
	if _, err := tx.Exec(ctx, `WITH wallclock AS (SELECT clock_timestamp() AS t)
		UPDATE attendance_sessions SET status='open', starts_at=wallclock.t,
		ends_at=wallclock.t + duration_minutes * interval '1 minute', updated_at=wallclock.t
		FROM wallclock WHERE id=$1`, session.ID); err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "attendance.session_opened", "attendance_session", session.ID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CloseAttendanceSession(ctx context.Context, actor User, sessionID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return err
	}
	if session.Status == "closed" {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE attendance_sessions SET status='closed',updated_at=clock_timestamp() WHERE id=$1`, session.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE acoustic_challenges SET revoked_at=clock_timestamp()
		WHERE session_id=$1 AND revoked_at IS NULL`, session.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE submission_nonces SET used_at=clock_timestamp()
		WHERE session_id=$1 AND used_at IS NULL`, session.ID); err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "attendance.session_closed", "attendance_session", session.ID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Expiry is derived from PostgreSQL's wall clock; it never reopens a session.
const attendanceReadColumns = `a.id::text, a.course_id::text, a.room_id::text, r.name, a.title,
	CASE WHEN a.status='open' AND a.ends_at <= clock_timestamp() THEN 'expired' ELSE a.status END,
	a.policy_version, a.starts_at, a.ends_at, a.duration_minutes`

func (s *Store) ListAttendanceSessions(ctx context.Context, user User, courseID string) ([]AttendanceSession, error) {
	if _, err := s.Course(ctx, user, courseID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+attendanceReadColumns+` FROM attendance_sessions a
		JOIN rooms r ON r.id=a.room_id JOIN courses c ON c.id=a.course_id JOIN users u ON u.id=$1 AND u.active
		WHERE c.id=$2 AND (`+coursePermission+`) ORDER BY a.created_at DESC,a.id LIMIT 100`, user.ID, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AttendanceSession, 0)
	for rows.Next() {
		session, err := scanAttendanceSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (s *Store) AttendanceSession(ctx context.Context, user User, sessionID string) (AttendanceSession, error) {
	id, err := parameterUUID(sessionID)
	if err != nil {
		return AttendanceSession{}, err
	}
	actor, err := parameterUUID(user.ID)
	if err != nil {
		return AttendanceSession{}, err
	}
	return scanAttendanceSession(s.Pool.QueryRow(ctx, `SELECT `+attendanceReadColumns+` FROM attendance_sessions a
		JOIN rooms r ON r.id=a.room_id JOIN courses c ON c.id=a.course_id JOIN users u ON u.id=$1 AND u.active
		WHERE a.id=$2 AND (`+coursePermission+`)`, actor, id))
}

func (s *Store) AttendanceRooms(ctx context.Context, actor User, courseID string) ([]Room, error) {
	id, err := parameterUUID(courseID)
	if err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	role, err := lockAttendanceUser(ctx, tx, actor, false)
	if err != nil {
		return nil, err
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id=$1 FOR SHARE`, id).Scan(&found); err != nil {
		return nil, err
	}
	if err := lockAttendanceMembership(ctx, tx, actor, role, courseID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text,name,kind,capacity FROM rooms ORDER BY name,id LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rooms := make([]Room, 0)
	for rows.Next() {
		var room Room
		if err := rows.Scan(&room.ID, &room.Name, &room.Kind, &room.Capacity); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

const attendanceResultColumns = `u.id::text,u.email,u.name,
	COALESCE(ar.status,va.decision,'not_submitted'), COALESCE(ar.source,CASE WHEN va.id IS NOT NULL THEN 'challenge' END,''),
	COALESCE(ar.reason_code,va.reason_code,''), COALESCE(rv.reason,''), COALESCE(ar.reviewed_by::text,''), COALESCE(reviewer.name,''),
	COALESCE(ar.updated_at,va.created_at,'0001-01-01 00:00:00+00'::timestamptz),
	(u.role='student' AND u.active AND COALESCE(e.status='active',false))`

const attendanceResultJoins = `LEFT JOIN attendance_records ar ON ar.session_id=$1 AND ar.student_id=u.id
	LEFT JOIN LATERAL (SELECT id,decision,reason_code,created_at FROM verification_attempts
		WHERE session_id=$1 AND student_id=u.id ORDER BY created_at DESC,id DESC LIMIT 1) va ON true
	LEFT JOIN LATERAL (SELECT id,reason FROM attendance_reviews
		WHERE session_id=$1 AND student_id=u.id ORDER BY created_at DESC,id DESC LIMIT 1) rv ON true
	LEFT JOIN users reviewer ON reviewer.id=ar.reviewed_by`

func scanAttendanceResult(row pgx.Row) (AttendanceResult, error) {
	var result AttendanceResult
	err := row.Scan(&result.StudentID, &result.Email, &result.Name, &result.Status, &result.Source, &result.ReasonCode,
		&result.ReviewReason, &result.ReviewedBy, &result.ReviewerName, &result.UpdatedAt, &result.CanReview)
	return result, err
}

func (s *Store) AttendanceResults(ctx context.Context, actor User, sessionID string) ([]AttendanceResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+attendanceResultColumns+` FROM users u
		LEFT JOIN enrollments e ON e.student_id=u.id AND e.course_id=$2
		`+attendanceResultJoins+` WHERE (u.role='student' AND u.active AND e.status='active')
		OR ar.id IS NOT NULL OR va.id IS NOT NULL OR rv.id IS NOT NULL
		ORDER BY u.name,u.email,u.id LIMIT 100`, session.ID, session.CourseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]AttendanceResult, 0)
	for rows.Next() {
		result, err := scanAttendanceResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (s *Store) MyAttendance(ctx context.Context, user User, sessionID string) (AttendanceResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceResult{}, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceStudent(ctx, tx, user, sessionID)
	if err != nil {
		return AttendanceResult{}, err
	}
	return scanAttendanceResult(tx.QueryRow(ctx, `SELECT `+attendanceResultColumns+` FROM users u
		LEFT JOIN enrollments e ON e.student_id=u.id AND e.course_id=$3
		`+attendanceResultJoins+` WHERE u.id=$2`, session.ID, user.ID, session.CourseID))
}

// Review appends a human decision while preserving original verification evidence.
// A reason is mandatory, including for students using the manual fallback.
func (s *Store) ReviewAttendance(ctx context.Context, actor User, sessionID, studentEmail, decision, reason string) error {
	reason = strings.TrimSpace(reason)
	if (decision != "present" && decision != "rejected") || reason == "" || len(reason) > 500 || !utf8.ValidString(reason) {
		return ErrAttendanceInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return err
	}
	if session.StartsAt.IsZero() || (session.Status != "open" && session.Status != "closed") {
		return ErrAttendanceState
	}
	studentID, err := activeTarget(ctx, tx, studentEmail, "student")
	if err != nil {
		return err
	}
	if err := lockAttendanceMembership(ctx, tx, User{ID: studentID}, "student", session.CourseID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attendance_reviews(session_id,student_id,actor_id,decision,reason,created_at)
		VALUES($1,$2,$3,$4,$5,clock_timestamp())`, session.ID, studentID, actor.ID, decision, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attendance_records(session_id,student_id,status,source,reason_code,reviewed_by,updated_at)
		VALUES($1,$2,$3,'manual','manual_review',$4,clock_timestamp())
		ON CONFLICT(session_id,student_id) DO UPDATE SET status=excluded.status,source='manual',reason_code='manual_review',
		reviewed_by=excluded.reviewed_by,updated_at=clock_timestamp()`, session.ID, studentID, decision, actor.ID); err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "attendance.reviewed", "attendance_session", session.ID,
		map[string]string{"student_id": studentID, "decision": decision, "reason": reason, "source": "manual", "policy_version": session.PolicyVersion}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
