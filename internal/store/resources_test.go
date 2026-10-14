package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func resourceMetadata(courseID, key string) Resource {
	return Resource{CourseID: courseID, Title: "Lecture notes", OriginalName: "notes.txt", StorageKey: strings.Repeat(key, 64), MIMEType: "text/plain", SizeBytes: 12}
}

func TestResourcePermissionsAndAudit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@resources.example", "admin")
	lecturer := addUser(t, s, "lecturer@resources.example", "lecturer")
	student := addUser(t, s, "student@resources.example", "student")
	stranger := addUser(t, s, "stranger@resources.example", "student")
	course := addCourse(t, s, "RES101")
	other := addCourse(t, s, "RES202")
	mustExec(t, s, `INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)`, course.ID, lecturer.ID)
	mustExec(t, s, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)`, course.ID, student.ID)
	resource, err := s.CreateResource(ctx, lecturer, resourceMetadata(course.ID, "a"))
	if err != nil || resource.ID == "" {
		t.Fatalf("resource=%+v error=%v", resource, err)
	}
	for _, user := range []User{admin, lecturer, student} {
		got, err := s.Resource(ctx, user, resource.ID)
		if err != nil || got != resource {
			t.Fatalf("authorized %s resource=%+v error=%v", user.Role, got, err)
		}
		list, err := s.Resources(ctx, user, course.ID)
		if err != nil || len(list) != 1 || list[0] != resource {
			t.Fatalf("resource list=%v err=%v", list, err)
		}
	}
	for _, user := range []User{stranger, {ID: stranger.ID, Role: "admin"}} {
		if _, err := s.Resource(ctx, user, resource.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stranger access=%v", err)
		}
		if _, err := s.Resources(ctx, user, course.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stranger list=%v", err)
		}
	}
	for _, user := range []User{student, stranger, {ID: student.ID, Role: "admin"}} {
		if _, err := s.UploadCourse(ctx, user, course.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("student upload=%v", err)
		}
		if _, err := s.CreateResource(ctx, user, resourceMetadata(course.ID, "b")); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("student insert=%v", err)
		}
	}
	if _, err := s.CreateResource(ctx, lecturer, resourceMetadata(other.ID, "b")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unassigned insert=%v", err)
	}
	if _, err := s.CreateResource(ctx, admin, resourceMetadata(other.ID, "c")); err != nil {
		t.Fatal(err)
	}
	var auditCount int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='resource.uploaded' AND actor_id=$1 AND target_id=$2 AND metadata->>'course_id'=$3`, lecturer.ID, resource.ID, course.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("audit=%d err=%v", auditCount, err)
	}
	for _, id := range []string{"", "invalid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.Resource(ctx, admin, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid id=%q err=%v", id, err)
		}
	}
	mustExec(t, s, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, student.ID)
	if _, err := s.Resource(ctx, student, resource.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn access=%v", err)
	}
	mustExec(t, s, `DELETE FROM course_lecturers WHERE lecturer_id=$1`, lecturer.ID)
	if _, err := s.CreateResource(ctx, lecturer, resourceMetadata(course.ID, "b")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reassigned lecturer inserted=%v", err)
	}
	if _, err := s.Resource(ctx, lecturer, resource.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reassigned lecturer access=%v", err)
	}
	mustExec(t, s, `UPDATE users SET active=false WHERE id=$1`, admin.ID)
	if _, err := s.Resource(ctx, admin, resource.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive administrator access=%v", err)
	}
	if _, err := s.CreateResource(ctx, admin, resourceMetadata(course.ID, "b")); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive administrator inserted=%v", err)
	}
}

func TestResourceAuditFailureRollsBackMetadata(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@rollback.example", "admin")
	course := addCourse(t, s, "RES101")
	mustExec(t, s, `ALTER TABLE audit_events ADD CONSTRAINT reject_upload CHECK(action <> 'resource.uploaded')`)
	if _, err := s.CreateResource(ctx, admin, resourceMetadata(course.ID, "a")); postgresCode(err) != "23514" {
		t.Fatalf("audit failure error=%v", err)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM resources`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed audit persisted metadata count=%d error=%v", count, err)
	}
	invalid := resourceMetadata(course.ID, "a")
	invalid.StorageKey = "../outside"
	if _, err := s.CreateResource(ctx, admin, invalid); !errors.Is(err, ErrInvalidResource) {
		t.Fatalf("unsafe metadata error=%v", err)
	}
}
