package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCatalogCRUDAndValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	course, err := s.SaveCourse(ctx, admin, "", " CSC101 ", " Introduction to computing ")
	if err != nil || course.ID == "" || course.Code != "CSC101" || course.Title != "Introduction to computing" {
		t.Fatalf("created course=%v error=%v", course, err)
	}
	updated, err := s.SaveCourse(ctx, admin, course.ID, "CSC102", "Computing foundations")
	if err != nil || updated.ID != course.ID || updated.Code != "CSC102" || updated.Title != "Computing foundations" {
		t.Fatalf("updated course=%v error=%v", updated, err)
	}
	for _, tc := range []struct {
		name, id, code, title, wantCode string
	}{
		{"duplicate", "", "CSC102", "Other title", "23505"},
		{"blank code", "", "  ", "Title", "23514"},
		{"blank title", "", "CSC201", "  ", "23514"},
		{"blank update title", course.ID, "CSC102", "  ", "23514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SaveCourse(ctx, admin, tc.id, tc.code, tc.title); postgresCode(err) != tc.wantCode {
				t.Fatalf("error=%v want PostgreSQL code=%s", err, tc.wantCode)
			}
		})
	}
	for _, id := range []string{"invalid-id", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.SaveCourse(ctx, admin, id, "CSC300", "Title"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid/missing update id=%q error=%v", id, err)
		}
	}
	room, err := s.SaveRoom(ctx, admin, "", " Room 1 ", "room", 40)
	if err != nil || room.ID == "" || room.Name != "Room 1" || room.Kind != "room" || room.Capacity != 40 {
		t.Fatalf("created room=%v error=%v", room, err)
	}
	updatedRoom, err := s.SaveRoom(ctx, admin, room.ID, "Theatre 1", "theatre", 500)
	if err != nil || updatedRoom.ID != room.ID || updatedRoom.Name != "Theatre 1" || updatedRoom.Kind != "theatre" || updatedRoom.Capacity != 500 {
		t.Fatalf("updated room=%v error=%v", updatedRoom, err)
	}
	for _, tc := range []struct {
		name, roomName, kind, wantCode string
		capacity                       int
	}{
		{"duplicate room", "Theatre 1", "room", "23505", 30},
		{"blank name", " ", "room", "23514", 30},
		{"invalid kind", "Room 2", "hall", "23514", 30},
		{"zero capacity", "Room 2", "room", "23514", 0},
		{"negative capacity", "Room 2", "room", "23514", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SaveRoom(ctx, admin, "", tc.roomName, tc.kind, tc.capacity); postgresCode(err) != tc.wantCode {
				t.Fatalf("error=%v want PostgreSQL code=%s", err, tc.wantCode)
			}
		})
	}
	for _, id := range []string{"invalid-id", "00000000-0000-0000-0000-000000000000"} {
		if _, err := s.SaveRoom(ctx, admin, id, "Room 2", "room", 40); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid/missing room id=%q error=%v", id, err)
		}
	}
	rooms, err := s.Rooms(ctx, admin)
	if err != nil || len(rooms) != 1 || rooms[0] != updatedRoom {
		t.Fatalf("rooms=%v error=%v", rooms, err)
	}
	if got, err := s.Course(ctx, admin, course.ID); err != nil || got != updated {
		t.Fatalf("failed updates changed course: %v error=%v", got, err)
	}
	assertAuditCount(t, s, 4)
	var attributed int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE actor_id = $1 AND ((target_type = 'course' AND target_id = $2)
		OR (target_type = 'room' AND target_id = $3)) AND metadata = '{}'::jsonb`, admin.ID, course.ID, room.ID).Scan(&attributed); err != nil || attributed != 4 {
		t.Fatalf("attributed audit count=%d error=%v", attributed, err)
	}
}

func TestCatalogRequiresCurrentActiveAdministrator(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	course := addCourse(t, s, "CSC101")
	actors := []User{lecturer, student, {ID: student.ID, Role: "admin"}, {ID: "malformed", Role: "admin"}}
	assertDenied := func(actor User) {
		t.Helper()
		if _, err := s.SaveCourse(ctx, actor, "", "CSC202", "Title"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized course mutation actor=%v error=%v", actor, err)
		}
		if _, err := s.SaveRoom(ctx, actor, "", "Room 1", "room", 40); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized room mutation actor=%v error=%v", actor, err)
		}
		if _, err := s.Rooms(ctx, actor); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized room listing actor=%v error=%v", actor, err)
		}
		if err := s.AssignLecturer(ctx, actor, course.ID, lecturer.Email, true); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized assignment actor=%v error=%v", actor, err)
		}
		if err := s.SetEnrollment(ctx, actor, course.ID, student.Email, "active"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized enrollment actor=%v error=%v", actor, err)
		}
		if _, err := s.CourseMembers(ctx, actor, course.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized roster actor=%v error=%v", actor, err)
		}
	}
	for _, actor := range actors {
		assertDenied(actor)
	}
	mustExec(t, s, `UPDATE users SET role = 'student' WHERE id = $1`, admin.ID)
	assertDenied(admin)
	mustExec(t, s, `UPDATE users SET role = 'admin', active = false WHERE id = $1`, admin.ID)
	assertDenied(admin)
	assertAuditCount(t, s, 0)
}

func TestRosterProvisioningRevokesAccessImmediately(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	course := addCourse(t, s, "CSC101")
	for i := 0; i < 2; i++ {
		if err := s.AssignLecturer(ctx, admin, course.ID, " LECTURER@Test.Example ", true); err != nil {
			t.Fatalf("assignment attempt %d: %v", i, err)
		}
		if err := s.SetEnrollment(ctx, admin, course.ID, " STUDENT@Test.Example ", "active"); err != nil {
			t.Fatalf("enrollment attempt %d: %v", i, err)
		}
	}
	for _, user := range []User{lecturer, student} {
		if _, err := s.Course(ctx, user, course.ID); err != nil {
			t.Fatalf("provisioned role=%s has no course access: %v", user.Role, err)
		}
	}
	members, err := s.CourseMembers(ctx, admin, course.ID)
	if err != nil || len(members) != 2 || members[0].ID != lecturer.ID || members[0].Status != "assigned" || members[1].ID != student.ID || members[1].Status != "active" {
		t.Fatalf("members=%v error=%v", members, err)
	}
	if err := s.AssignLecturer(ctx, admin, course.ID, lecturer.Email, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnrollment(ctx, admin, course.ID, student.Email, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	for _, user := range []User{lecturer, student} {
		if _, err := s.Course(ctx, user, course.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("revoked role=%s retains course access: %v", user.Role, err)
		}
	}
	members, err = s.CourseMembers(ctx, admin, course.ID)
	if err != nil || len(members) != 1 || members[0].ID != student.ID || members[0].Status != "withdrawn" {
		t.Fatalf("withdrawn roster=%v error=%v", members, err)
	}
	if err := s.SetEnrollment(ctx, admin, course.ID, student.Email, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Course(ctx, student, course.ID); err != nil {
		t.Fatalf("reenrollment did not restore course: %v", err)
	}
	assertAuditCount(t, s, 7)
	var lecturerTarget, studentTarget string
	if err := s.Pool.QueryRow(ctx, `SELECT metadata->>'lecturer_id' FROM audit_events WHERE action = 'lecturer.remove' AND actor_id = $1 AND target_id = $2`, admin.ID, course.ID).Scan(&lecturerTarget); err != nil || lecturerTarget != lecturer.ID {
		t.Fatalf("lecturer removal attribution=%q error=%v", lecturerTarget, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT metadata->>'student_id' FROM audit_events WHERE action = 'enrollment.withdrawn' AND actor_id = $1 AND target_id = $2`, admin.ID, course.ID).Scan(&studentTarget); err != nil || studentTarget != student.ID {
		t.Fatalf("student withdrawal attribution=%q error=%v", studentTarget, err)
	}
}

func TestRosterRejectsInvalidTargetsAndEnums(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	course := addCourse(t, s, "CSC101")
	for _, email := range []string{admin.Email, student.Email, "missing@test.example"} {
		for _, assigned := range []bool{true, false} {
			if err := s.AssignLecturer(ctx, admin, course.ID, email, assigned); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid lecturer email=%q assigned=%v error=%v", email, assigned, err)
			}
		}
	}
	for _, email := range []string{admin.Email, lecturer.Email, "missing@test.example"} {
		for _, status := range []string{"active", "withdrawn"} {
			if err := s.SetEnrollment(ctx, admin, course.ID, email, status); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("invalid student email=%q status=%q error=%v", email, status, err)
			}
		}
	}
	if err := s.SetEnrollment(ctx, admin, course.ID, student.Email, "pending"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid enrollment enum error=%v", err)
	}
	for _, id := range []string{"not-a-uuid", "", "00000000-0000-0000-0000-000000000000"} {
		if err := s.AssignLecturer(ctx, admin, id, lecturer.Email, true); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid assignment course=%q error=%v", id, err)
		}
		if err := s.SetEnrollment(ctx, admin, id, student.Email, "active"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid enrollment course=%q error=%v", id, err)
		}
		if _, err := s.CourseMembers(ctx, admin, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid roster course=%q error=%v", id, err)
		}
	}
	mustExec(t, s, `UPDATE users SET active = false WHERE id IN ($1, $2)`, lecturer.ID, student.ID)
	if err := s.AssignLecturer(ctx, admin, course.ID, lecturer.Email, true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive lecturer assigned: %v", err)
	}
	if err := s.SetEnrollment(ctx, admin, course.ID, student.Email, "active"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("inactive student enrolled: %v", err)
	}
	assertAuditCount(t, s, 0)
}

func TestAuditFailureRollsBackCatalogMutations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	admin := addUser(t, s, "admin@test.example", "admin")
	lecturer := addUser(t, s, "lecturer@test.example", "lecturer")
	student := addUser(t, s, "student@test.example", "student")
	course := addCourse(t, s, "CSC101")
	var roomID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO rooms(name, kind, capacity) VALUES ('Room 1', 'room', 40) RETURNING id::text`).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `ALTER TABLE audit_events ADD CONSTRAINT reject_future CHECK (false) NOT VALID`)
	if _, err := s.SaveCourse(ctx, admin, "", "CSC202", "New"); postgresCode(err) != "23514" {
		t.Fatalf("course create audit failure=%v", err)
	}
	if _, err := s.SaveCourse(ctx, admin, course.ID, "CSC102", "Changed"); postgresCode(err) != "23514" {
		t.Fatalf("course update audit failure=%v", err)
	}
	if _, err := s.SaveRoom(ctx, admin, "", "Room 2", "room", 80); postgresCode(err) != "23514" {
		t.Fatalf("room create audit failure=%v", err)
	}
	if _, err := s.SaveRoom(ctx, admin, roomID, "Theatre 1", "theatre", 400); postgresCode(err) != "23514" {
		t.Fatalf("room update audit failure=%v", err)
	}
	if err := s.AssignLecturer(ctx, admin, course.ID, lecturer.Email, true); postgresCode(err) != "23514" {
		t.Fatalf("lecturer assignment audit failure=%v", err)
	}
	if err := s.SetEnrollment(ctx, admin, course.ID, student.Email, "active"); postgresCode(err) != "23514" {
		t.Fatalf("enrollment audit failure=%v", err)
	}
	if got, err := s.Course(ctx, admin, course.ID); err != nil || got != course {
		t.Fatalf("audit failure changed course=%v error=%v", got, err)
	}
	rooms, err := s.Rooms(ctx, admin)
	if err != nil || len(rooms) != 1 || rooms[0].Name != "Room 1" || rooms[0].Kind != "room" || rooms[0].Capacity != 40 {
		t.Fatalf("audit failure changed rooms=%v error=%v", rooms, err)
	}
	courses, err := s.Courses(ctx, admin)
	if err != nil || len(courses) != 1 {
		t.Fatalf("audit failure created course=%v error=%v", courses, err)
	}
	members, err := s.CourseMembers(ctx, admin, course.ID)
	if err != nil || len(members) != 0 {
		t.Fatalf("audit failure changed membership=%v error=%v", members, err)
	}
	assertAuditCount(t, s, 0)
}

func TestResourceSchemaConstraints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	user := addUser(t, s, "lecturer@test.example", "lecturer")
	course := addCourse(t, s, "CSC101")
	for _, tc := range []struct {
		name, title, originalName, storageKey, mime string
		size                                        int64
	}{
		{"blank title", " ", "notes.pdf", "key1", "application/pdf", 10},
		{"blank original name", "Notes", " ", "key2", "application/pdf", 10},
		{"blank storage key", "Notes", "notes.pdf", " ", "application/pdf", 10},
		{"unsupported mime", "Notes", "notes.exe", "key3", "application/octet-stream", 10},
		{"empty file", "Notes", "notes.pdf", "key4", "application/pdf", 0},
		{"oversized file", "Notes", "notes.pdf", "key5", "application/pdf", 5242881},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Pool.Exec(ctx, `INSERT INTO resources(course_id, uploaded_by, title, original_name, storage_key, mime_type, size_bytes)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, course.ID, user.ID, tc.title, tc.originalName, tc.storageKey, tc.mime, tc.size)
			if postgresCode(err) != "23514" {
				t.Fatalf("error=%v want check violation", err)
			}
		})
	}
	mustExec(t, s, `INSERT INTO resources(course_id, uploaded_by, title, original_name, storage_key, mime_type, size_bytes)
		VALUES ($1, $2, 'Notes', 'notes.txt', 'key-valid', 'text/plain', 1)`, course.ID, user.ID)
	if _, err := s.Pool.Exec(ctx, `INSERT INTO resources(course_id, uploaded_by, title, original_name, storage_key, mime_type, size_bytes)
		VALUES ($1, $2, 'Notes', 'notes.pdf', 'key-valid', 'application/pdf', 5242880)`, course.ID, user.ID); postgresCode(err) != "23505" {
		t.Fatalf("duplicate storage key error=%v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO resources(course_id, uploaded_by, title, original_name, storage_key, mime_type, size_bytes)
		VALUES ('00000000-0000-0000-0000-000000000000', $1, 'Notes', 'notes.pdf', 'key-fk', 'application/pdf', 1)`, user.ID); postgresCode(err) != "23503" {
		t.Fatalf("missing resource course error=%v", err)
	}
}

func assertAuditCount(t *testing.T, s *Store, want int) {
	t.Helper()
	var count int
	if err := s.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events`).Scan(&count); err != nil || count != want {
		t.Fatalf("audit count=%d want=%d error=%v", count, want, err)
	}
}
