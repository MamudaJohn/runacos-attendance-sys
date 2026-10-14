package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Room struct {
	ID, Name, Kind string
	Capacity       int
}

type Member struct {
	ID, Email, Name, Role, Status string
}

// parameterUUID makes malformed identifiers indistinguishable from missing rows.
func parameterUUID(id string) (pgtype.UUID, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil || !uuid.Valid {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return uuid, nil
}

// The lock prevents the account from being disabled or demoted before commit.
// Caller-provided role fields never grant authority.
func requireCurrentAdmin(ctx context.Context, tx pgx.Tx, actor User) error {
	id, err := parameterUUID(actor.ID)
	if err != nil {
		return err
	}
	var currentID string
	return tx.QueryRow(ctx, `SELECT id::text FROM users
		WHERE id = $1 AND active AND role = 'admin' FOR SHARE`, id).Scan(&currentID)
}

func auditMutation(ctx context.Context, tx pgx.Tx, actorID, action, targetType, targetID string, metadata any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (actor_id, action, target_type, target_id, metadata)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, actorID, action, targetType, targetID, encoded)
	return err
}

// SaveCourse creates a course when id is empty, or updates the indicated course.
func (s *Store) SaveCourse(ctx context.Context, actor User, id, code, title string) (Course, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Course{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return Course{}, err
	}
	var course Course
	action := "course.create"
	code, title = strings.TrimSpace(code), strings.TrimSpace(title)
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO courses (code, title) VALUES ($1, $2)
			RETURNING id::text, code, title`, code, title).Scan(&course.ID, &course.Code, &course.Title)
	} else {
		var uuid pgtype.UUID
		uuid, err = parameterUUID(id)
		if err != nil {
			return Course{}, err
		}
		action = "course.update"
		err = tx.QueryRow(ctx, `UPDATE courses SET code = $2, title = $3, updated_at = now()
			WHERE id = $1 RETURNING id::text, code, title`, uuid, code, title).
			Scan(&course.ID, &course.Code, &course.Title)
	}
	if err != nil {
		return Course{}, err
	}
	if err := auditMutation(ctx, tx, actor.ID, action, "course", course.ID, nil); err != nil {
		return Course{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Course{}, err
	}
	return course, nil
}

func (s *Store) SaveRoom(ctx context.Context, actor User, id, name, kind string, capacity int) (Room, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Room{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return Room{}, err
	}
	var room Room
	action := "room.create"
	name = strings.TrimSpace(name)
	if id == "" {
		err = tx.QueryRow(ctx, `INSERT INTO rooms (name, kind, capacity) VALUES ($1, $2, $3)
			RETURNING id::text, name, kind, capacity`, name, kind, capacity).
			Scan(&room.ID, &room.Name, &room.Kind, &room.Capacity)
	} else {
		var uuid pgtype.UUID
		uuid, err = parameterUUID(id)
		if err != nil {
			return Room{}, err
		}
		action = "room.update"
		err = tx.QueryRow(ctx, `UPDATE rooms SET name = $2, kind = $3, capacity = $4, updated_at = now()
			WHERE id = $1 RETURNING id::text, name, kind, capacity`, uuid, name, kind, capacity).
			Scan(&room.ID, &room.Name, &room.Kind, &room.Capacity)
	}
	if err != nil {
		return Room{}, err
	}
	if err := auditMutation(ctx, tx, actor.ID, action, "room", room.ID, nil); err != nil {
		return Room{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Room{}, err
	}
	return room, nil
}

func (s *Store) Rooms(ctx context.Context, actor User) ([]Room, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text, name, kind, capacity FROM rooms ORDER BY name, id LIMIT 100`)
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

func requireCourse(ctx context.Context, tx pgx.Tx, courseID string) error {
	id, err := parameterUUID(courseID)
	if err != nil {
		return err
	}
	var found string
	return tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id = $1 FOR UPDATE`, id).Scan(&found)
}

func activeTarget(ctx context.Context, tx pgx.Tx, email, role string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM users
		WHERE email = $1 AND role = $2 AND active FOR SHARE`, strings.ToLower(strings.TrimSpace(email)), role).Scan(&id)
	return id, err
}

func (s *Store) AssignLecturer(ctx context.Context, actor User, courseID, email string, assigned bool) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return err
	}
	if err := requireCourse(ctx, tx, courseID); err != nil {
		return err
	}
	lecturerID, err := activeTarget(ctx, tx, email, "lecturer")
	if err != nil {
		return err
	}
	action := "lecturer.assign"
	if assigned {
		_, err = tx.Exec(ctx, `INSERT INTO course_lecturers (course_id, lecturer_id) VALUES ($1, $2)
			ON CONFLICT (course_id, lecturer_id) DO NOTHING`, courseID, lecturerID)
	} else {
		action = "lecturer.remove"
		_, err = tx.Exec(ctx, `DELETE FROM course_lecturers WHERE course_id = $1 AND lecturer_id = $2`, courseID, lecturerID)
	}
	if err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, action, "course", courseID, map[string]string{"lecturer_id": lecturerID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SetEnrollment(ctx context.Context, actor User, courseID, email, status string) error {
	if status != "active" && status != "withdrawn" {
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
	if err := requireCourse(ctx, tx, courseID); err != nil {
		return err
	}
	studentID, err := activeTarget(ctx, tx, email, "student")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO enrollments (course_id, student_id, status) VALUES ($1, $2, $3)
		ON CONFLICT (course_id, student_id) DO UPDATE SET status = excluded.status`, courseID, studentID, status); err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "enrollment."+status, "course", courseID, map[string]string{"student_id": studentID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CourseMembers is an administrative roster, including withdrawn students.
func (s *Store) CourseMembers(ctx context.Context, actor User, courseID string) ([]Member, error) {
	id, err := parameterUUID(courseID)
	if err != nil {
		return nil, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := requireCurrentAdmin(ctx, tx, actor); err != nil {
		return nil, err
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id = $1`, id).Scan(&found); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT u.id::text, u.email, u.name, u.role, 'assigned' AS status
		FROM course_lecturers cl JOIN users u ON u.id = cl.lecturer_id WHERE cl.course_id = $1
		UNION ALL
		SELECT u.id::text, u.email, u.name, u.role, e.status
		FROM enrollments e JOIN users u ON u.id = e.student_id WHERE e.course_id = $1
		ORDER BY role, email, id LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make([]Member, 0)
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.ID, &member.Email, &member.Name, &member.Role, &member.Status); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}
