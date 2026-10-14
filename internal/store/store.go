// Package store contains the PostgreSQL queries for accounts and course access.
package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type User struct {
	ID, Email, Name, Role string
}

type Course struct {
	ID    string `json:"id"`
	Code  string `json:"code"`
	Title string `json:"title"`
}

type Store struct {
	Pool *pgxpool.Pool
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, string, error) {
	var user User
	var hash string
	err := s.Pool.QueryRow(ctx, `SELECT id::text, email, name, role, password_hash
		FROM users WHERE email = $1 AND active`, strings.ToLower(strings.TrimSpace(email))).
		Scan(&user.ID, &user.Email, &user.Name, &user.Role, &hash)
	return user, hash, err
}

func (s *Store) Session(ctx context.Context, hash string) (User, error) {
	var user User
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text, u.email, u.name, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.active`, hash).
		Scan(&user.ID, &user.Email, &user.Name, &user.Role)
	return user, err
}

// CreateSession only permits an active account. Tokens are hashed before this call.
func (s *Store) CreateSession(ctx context.Context, userID, hash string, expires time.Time) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, expires_at)
		SELECT id, $2, $3 FROM users WHERE id = $1 AND active`, userID, hash, expires)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return tx.Commit(ctx)
}

// CreateUser rechecks administrator authority in the same statement as insertion.
func (s *Store) CreateUser(ctx context.Context, actor User, name, email, role, passwordHash string) (User, error) {
	var user User
	err := s.Pool.QueryRow(ctx, `INSERT INTO users (name, email, role, password_hash)
		SELECT $2, $3, $4, $5 FROM users WHERE id = $1 AND role = 'admin' AND active
		RETURNING id::text, email, name, role`, actor.ID, strings.TrimSpace(name),
		strings.ToLower(strings.TrimSpace(email)), role, passwordHash).
		Scan(&user.ID, &user.Email, &user.Name, &user.Role)
	return user, err
}

func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash)
	return err
}

// Course access derives the current account role from PostgreSQL, not caller fields.
// LIMIT bounds the initial dashboard; pagination can be added when needed.
func (s *Store) Courses(ctx context.Context, user User) ([]Course, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.id::text, c.code, c.title
		FROM courses c JOIN users u ON u.id = $1 AND u.active
		WHERE `+coursePermission+` ORDER BY c.code, c.id LIMIT 100`, user.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	courses := make([]Course, 0)
	for rows.Next() {
		var course Course
		if err := rows.Scan(&course.ID, &course.Code, &course.Title); err != nil {
			return nil, err
		}
		courses = append(courses, course)
	}
	return courses, rows.Err()
}

func (s *Store) Course(ctx context.Context, user User, id string) (Course, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		return Course{}, pgx.ErrNoRows
	}
	var course Course
	err := s.Pool.QueryRow(ctx, `SELECT c.id::text, c.code, c.title
		FROM courses c JOIN users u ON u.id = $1 AND u.active
		WHERE c.id = $2 AND (`+coursePermission+`)`, user.ID, uuid).
		Scan(&course.ID, &course.Code, &course.Title)
	return course, err
}

const coursePermission = `u.role = 'admin'
	OR (u.role = 'lecturer' AND EXISTS (
		SELECT 1 FROM course_lecturers cl WHERE cl.course_id = c.id AND cl.lecturer_id = u.id))
	OR (u.role = 'student' AND EXISTS (
		SELECT 1 FROM enrollments e WHERE e.course_id = c.id AND e.student_id = u.id AND e.status = 'active'))`
