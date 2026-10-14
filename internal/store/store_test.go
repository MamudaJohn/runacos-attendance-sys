package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; requires disposable PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "cabv_store_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		admin.Close()
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(ctx, db, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Store{Pool: pool}
}

func addUser(t *testing.T, s *Store, email, role string) User {
	t.Helper()
	var user User
	if err := s.Pool.QueryRow(context.Background(), `INSERT INTO users (email, name, password_hash, role)
		VALUES ($1, $1, 'test-hash', $2) RETURNING id::text, email, name, role`, email, role).
		Scan(&user.ID, &user.Email, &user.Name, &user.Role); err != nil {
		t.Fatal(err)
	}
	return user
}

func addCourse(t *testing.T, s *Store, code string) Course {
	t.Helper()
	var course Course
	if err := s.Pool.QueryRow(context.Background(), `INSERT INTO courses (code, title)
		VALUES ($1, $1) RETURNING id::text, code, title`, code).
		Scan(&course.ID, &course.Code, &course.Title); err != nil {
		t.Fatal(err)
	}
	return course
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestCoursePermissions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	stranger := addUser(t, s, "other@test.example", "student")
	course := addCourse(t, s, "CSC101")
	other := addCourse(t, s, "CSC202")
	mustExec(t, s, "INSERT INTO course_lecturers (course_id, lecturer_id) VALUES ($1, $2)", course.ID, lecturer.ID)
	mustExec(t, s, "INSERT INTO enrollments (course_id, student_id) VALUES ($1, $2)", course.ID, student.ID)
	mustExec(t, s, "INSERT INTO enrollments (course_id, student_id, status) VALUES ($1, $2, 'withdrawn')", other.ID, student.ID)
	for _, tc := range []struct {
		name string
		user User
		want int
	}{
		{"admin", admin, 2}, {"lecturer", lecturer, 1}, {"student", student, 1}, {"unrelated student", stranger, 0},
		{"forged admin role", User{ID: stranger.ID, Role: "admin"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			courses, err := s.Courses(ctx, tc.user)
			if err != nil || len(courses) != tc.want {
				t.Fatalf("courses=%v error=%v want count=%d", courses, err, tc.want)
			}
		})
	}
	for _, user := range []User{admin, lecturer, student} {
		if got, err := s.Course(ctx, user, course.ID); err != nil || got != course {
			t.Fatalf("authorized user %s course=%v error=%v", user.Role, got, err)
		}
	}
	for _, tc := range []struct {
		user User
		id   string
	}{
		{lecturer, other.ID}, {student, other.ID}, {stranger, course.ID}, {admin, "not-a-uuid"}, {admin, ""},
	} {
		if _, err := s.Course(ctx, tc.user, tc.id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized/invalid course %q user=%s error=%v", tc.id, tc.user.Role, err)
		}
	}
	mustExec(t, s, "UPDATE enrollments SET status='withdrawn' WHERE student_id=$1", student.ID)
	if _, err := s.Course(ctx, student, course.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn enrollment grants access: %v", err)
	}
	mustExec(t, s, "UPDATE users SET active=false WHERE id=$1", admin.ID)
	if courses, err := s.Courses(ctx, admin); err != nil || len(courses) != 0 {
		t.Fatalf("inactive admin courses=%v error=%v", courses, err)
	}
	if _, err := s.Course(ctx, admin, course.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive admin grants access: %v", err)
	}
}

func TestAccountAndSessionLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user := addUser(t, s, "student@test.example", "student")
	got, hash, err := s.UserByEmail(ctx, " STUDENT@Test.Example ")
	if err != nil || got != user || hash != "test-hash" {
		t.Fatalf("login lookup=%v hash=%q error=%v", got, hash, err)
	}
	if _, _, err := s.UserByEmail(ctx, "missing@test.example"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing user: %v", err)
	}
	if err := s.CreateSession(ctx, user.ID, "valid-hash", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Session(ctx, "valid-hash"); err != nil || got != user {
		t.Fatalf("valid session=%v error=%v", got, err)
	}
	if err := s.CreateSession(ctx, user.ID, "expired-hash", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "expired-hash"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired session: %v", err)
	}
	if err := s.CreateSession(ctx, user.ID, "next-hash", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var expiredCount int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_hash='expired-hash'").Scan(&expiredCount); err != nil || expiredCount != 0 {
		t.Fatalf("expired cleanup count=%d error=%v", expiredCount, err)
	}
	if err := s.DeleteSession(ctx, "valid-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "valid-hash"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("logged out session: %v", err)
	}
	if err := s.DeleteSession(ctx, "valid-hash"); err != nil {
		t.Fatalf("repeat logout: %v", err)
	}
	mustExec(t, s, "UPDATE users SET active=false WHERE id=$1", user.ID)
	if _, _, err := s.UserByEmail(ctx, user.Email); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive login: %v", err)
	}
	if _, err := s.Session(ctx, "next-hash"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive session: %v", err)
	}
	if err := s.CreateSession(ctx, user.ID, "inactive-hash", time.Now().Add(time.Hour)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive session creation: %v", err)
	}
}

func TestCreateUserRequiresActiveAdministrator(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	created, err := s.CreateUser(ctx, admin, " New Student ", " NEW@Test.Example ", "student", "password-hash")
	if err != nil || created.ID == "" || created.Name != "New Student" || created.Email != "new@test.example" || created.Role != "student" {
		t.Fatalf("created=%v error=%v", created, err)
	}
	for _, actor := range []User{lecturer, student, {ID: student.ID, Role: "admin"}} {
		if _, err := s.CreateUser(ctx, actor, "Other", "other@test.example", "student", "hash"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("nonadmin %s created user: %v", actor.ID, err)
		}
	}
	if _, err := s.CreateUser(ctx, admin, "Duplicate", created.Email, "student", "hash"); postgresCode(err) != "23505" {
		t.Fatalf("duplicate email error=%v", err)
	}
	mustExec(t, s, "UPDATE users SET active=false WHERE id=$1", admin.ID)
	if _, err := s.CreateUser(ctx, admin, "Other", "other@test.example", "student", "hash"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive admin created user: %v", err)
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := testStore(t)
	user := addUser(t, s, "student@test.example", "student")
	course := addCourse(t, s, "CSC101")
	for _, tc := range []struct {
		name, sql, code string
		args            []any
	}{
		{"uppercase email", "INSERT INTO users(email,name,password_hash,role) VALUES ('UPPER@test.example','Name','hash','student')", "23514", nil},
		{"invalid role", "INSERT INTO users(email,name,password_hash,role) VALUES ('role@test.example','Name','hash','owner')", "23514", nil},
		{"duplicate email", "INSERT INTO users(email,name,password_hash,role) VALUES ('student@test.example','Name','hash','student')", "23505", nil},
		{"bad enrollment status", "INSERT INTO enrollments(course_id,student_id,status) VALUES($1,$2,'pending')", "23514", []any{course.ID, user.ID}},
		{"missing course", "INSERT INTO enrollments(course_id,student_id) VALUES('00000000-0000-0000-0000-000000000000',$1)", "23503", []any{user.ID}},
		{"invalid room kind", "INSERT INTO rooms(name,kind,capacity) VALUES('A','hall',60)", "23514", nil},
		{"invalid room capacity", "INSERT INTO rooms(name,kind,capacity) VALUES('A','room',0)", "23514", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Pool.Exec(context.Background(), tc.sql, tc.args...)
			if postgresCode(err) != tc.code {
				t.Fatalf("error=%v want code=%s", err, tc.code)
			}
		})
	}
	mustExec(t, s, "INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)", course.ID, user.ID)
	if _, err := s.Pool.Exec(context.Background(), "INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)", course.ID, user.ID); postgresCode(err) != "23505" {
		t.Fatalf("duplicate enrollment: %v", err)
	}
}

func postgresCode(err error) string {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) {
		return pgerr.Code
	}
	return ""
}
