package store

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Resource struct {
	ID, CourseID, Title, OriginalName, StorageKey, MIMEType string
	SizeBytes                                               int64
}

var ErrInvalidResource = errors.New("invalid resource metadata")

const uploadPermission = `u.role = 'admin' OR (u.role = 'lecturer' AND EXISTS (
	SELECT 1 FROM course_lecturers cl WHERE cl.course_id = c.id AND cl.lecturer_id = u.id))`

func (s *Store) UploadCourse(ctx context.Context, user User, id string) (Course, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		return Course{}, pgx.ErrNoRows
	}
	var course Course
	err := s.Pool.QueryRow(ctx, `SELECT c.id::text,c.code,c.title FROM courses c
		JOIN users u ON u.id=$1 AND u.active WHERE c.id=$2 AND (`+uploadPermission+`)`, user.ID, uuid).
		Scan(&course.ID, &course.Code, &course.Title)
	return course, err
}

func (s *Store) Resources(ctx context.Context, user User, courseID string) ([]Resource, error) {
	if _, err := s.Course(ctx, user, courseID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT r.id::text,r.course_id::text,r.title,r.original_name,r.storage_key,r.mime_type,r.size_bytes
		FROM resources r JOIN courses c ON c.id=r.course_id JOIN users u ON u.id=$1 AND u.active
		WHERE c.id=$2 AND (`+coursePermission+`) ORDER BY r.created_at DESC,r.id LIMIT 100`, user.ID, courseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Resource, 0)
	for rows.Next() {
		var resource Resource
		if err := rows.Scan(&resource.ID, &resource.CourseID, &resource.Title, &resource.OriginalName, &resource.StorageKey, &resource.MIMEType, &resource.SizeBytes); err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, rows.Err()
}

func (s *Store) Resource(ctx context.Context, user User, id string) (Resource, error) {
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil {
		return Resource{}, pgx.ErrNoRows
	}
	var resource Resource
	err := s.Pool.QueryRow(ctx, `SELECT r.id::text,r.course_id::text,r.title,r.original_name,r.storage_key,r.mime_type,r.size_bytes
		FROM resources r JOIN courses c ON c.id=r.course_id JOIN users u ON u.id=$1 AND u.active
		WHERE r.id=$2 AND (`+coursePermission+`)`, user.ID, uuid).
		Scan(&resource.ID, &resource.CourseID, &resource.Title, &resource.OriginalName, &resource.StorageKey, &resource.MIMEType, &resource.SizeBytes)
	return resource, err
}

// CreateResource holds the account and assignment locks through insertion and
// audit recording. Reassignment/deactivation cannot race the authority check.
func (s *Store) CreateResource(ctx context.Context, actor User, resource Resource) (Resource, error) {
	resource.Title = strings.TrimSpace(resource.Title)
	if !validResourceMetadata(resource) {
		return Resource{}, ErrInvalidResource
	}
	var courseID pgtype.UUID
	if err := courseID.Scan(resource.CourseID); err != nil {
		return Resource{}, pgx.ErrNoRows
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Resource{}, err
	}
	defer tx.Rollback(ctx)
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND active FOR SHARE`, actor.ID).Scan(&role); err != nil {
		return Resource{}, err
	}
	if role != "admin" && role != "lecturer" {
		return Resource{}, pgx.ErrNoRows
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE id=$1 FOR SHARE`, courseID).Scan(&id); err != nil {
		return Resource{}, err
	}
	if role == "lecturer" {
		if err := tx.QueryRow(ctx, `SELECT course_id::text FROM course_lecturers WHERE course_id=$1 AND lecturer_id=$2 FOR SHARE`, courseID, actor.ID).Scan(&id); err != nil {
			return Resource{}, err
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO resources(course_id,uploaded_by,title,original_name,storage_key,mime_type,size_bytes)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`, courseID, actor.ID, resource.Title, resource.OriginalName, resource.StorageKey, resource.MIMEType, resource.SizeBytes).Scan(&resource.ID); err != nil {
		return Resource{}, err
	}
	if err := auditMutation(ctx, tx, actor.ID, "resource.uploaded", "resource", resource.ID, map[string]any{"course_id": resource.CourseID, "size_bytes": resource.SizeBytes, "mime_type": resource.MIMEType}); err != nil {
		return Resource{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Resource{}, err
	}
	return resource, nil
}

func validResourceMetadata(resource Resource) bool {
	if resource.Title == "" || len(resource.Title) > 200 || !utf8.ValidString(resource.Title) || len(resource.OriginalName) == 0 || len(resource.OriginalName) > 180 || !utf8.ValidString(resource.OriginalName) || resource.SizeBytes < 1 || resource.SizeBytes > 5242880 || len(resource.StorageKey) != 64 || (resource.MIMEType != "text/plain" && resource.MIMEType != "application/pdf") {
		return false
	}
	for _, r := range resource.StorageKey {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return !strings.ContainsAny(resource.OriginalName, "\\/\x00\r\n")
}
