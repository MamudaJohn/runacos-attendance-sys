// cabv-seed installs explicit demonstration data. It is never run by the server.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"cabv/internal/auth"
	"cabv/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return fmt.Errorf("usage: cabv-seed (development data only; set CABV_DEMO_PASSWORD and DATABASE_URL)")
	}
	password := os.Getenv("CABV_DEMO_PASSWORD")
	if len(password) < auth.MinPasswordBytes || len(password) > auth.MaxPasswordBytes {
		return fmt.Errorf("CABV_DEMO_PASSWORD must contain between 12 and 128 bytes; no default password is provided")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("cannot initialize database")
	}
	defer pool.Close()
	if err := seed(ctx, pool, password); err != nil {
		return fmt.Errorf("development seed failed: %w", err)
	}
	fmt.Println("Development data ready: admin@cabv.test, lecturer@cabv.test, student@cabv.test; CSC101, CSC202, Room A.")
	fmt.Println("Existing account passwords and account details are preserved. Never use these demo accounts for production.")
	return nil
}

func seed(ctx context.Context, pool *pgxpool.Pool, password string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	ids := make(map[string]string)
	for _, role := range []string{"admin", "lecturer", "student"} {
		hash, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		email := role + "@cabv.test"
		if _, err := tx.Exec(ctx, `INSERT INTO users (email, name, password_hash, role)
			VALUES ($1, $2, $3, $4) ON CONFLICT (email) DO NOTHING`, email, "Demo "+role, hash, role); err != nil {
			return err
		}
		var id, actualRole string
		var active bool
		if err := tx.QueryRow(ctx, `SELECT id::text, role, active FROM users WHERE email = $1`, email).
			Scan(&id, &actualRole, &active); err != nil {
			return err
		}
		if actualRole != role || !active {
			return fmt.Errorf("existing demo account %s has conflicting role or is disabled", email)
		}
		ids[role] = id
	}
	var courseID string
	for _, course := range []struct{ code, title string }{
		{"CSC101", "Introduction to Computer Science"},
		{"CSC202", "Data Structures"},
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO courses (code, title) VALUES ($1, $2)
			ON CONFLICT (code) DO NOTHING`, course.code, course.title); err != nil {
			return err
		}
		if course.code == "CSC101" {
			if err := tx.QueryRow(ctx, `SELECT id::text FROM courses WHERE code = $1`, course.code).Scan(&courseID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO course_lecturers (course_id, lecturer_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, courseID, ids["lecturer"]); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO enrollments (course_id, student_id)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, courseID, ids["student"]); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rooms (name, kind, capacity)
		VALUES ('Room A', 'room', 60) ON CONFLICT (name) DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
