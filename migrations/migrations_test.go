package migrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Each integration test owns a unique schema; TEST_DATABASE_URL should still be disposable.
func TestMigrationRoundTrip(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "cabv_migrations_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.ExecContext(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
	}()
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := goose.UpContext(ctx, db, "."); err != nil {
			t.Fatal(err)
		}
		if err := goose.UpContext(ctx, db, "."); err != nil {
			t.Fatalf("repeat up: %v", err)
		}
		var phase string
		if err := db.QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_phase'").Scan(&phase); err != nil || phase != "2a-experimental" {
			t.Fatalf("phase=%q error=%v", phase, err)
		}
		for _, table := range []string{"app_metadata", "users", "sessions", "courses", "course_lecturers", "enrollments", "rooms", "resources", "audit_events", "attendance_sessions", "acoustic_challenges", "submission_nonces", "verification_attempts", "attendance_records", "attendance_reviews", "identity_credentials", "identity_ceremonies", "identity_proofs", "room_calibrations", "attendance_evidence"} {
			var found sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", schema+"."+table).Scan(&found); err != nil || !found.Valid {
				t.Fatalf("expected table %s: %v error=%v", table, found, err)
			}
		}
		// Remove optional sensor evidence and identity first, preserving attendance.
		if err := goose.DownContext(ctx, db, "."); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_phase'").Scan(&phase); err != nil || phase != "1c" {
			t.Fatalf("sensor rollback phase=%q error=%v", phase, err)
		}
		if err := goose.DownContext(ctx, db, "."); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"identity_credentials", "identity_ceremonies", "identity_proofs", "room_calibrations", "attendance_evidence"} {
			var found sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", schema+"."+table).Scan(&found); err != nil || found.Valid {
				t.Fatalf("sensor/identity rollback left %s error=%v", table, err)
			}
		}
		// Attendance rollback preserves resources and accounts.
		if err := goose.DownContext(ctx, db, "."); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_phase'").Scan(&phase); err != nil || phase != "1b" {
			t.Fatalf("after 1C rollback phase=%q error=%v", phase, err)
		}
		for _, table := range []string{"attendance_sessions", "acoustic_challenges", "submission_nonces", "verification_attempts", "attendance_records", "attendance_reviews"} {
			var found sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", schema+"."+table).Scan(&found); err != nil || found.Valid {
				t.Fatalf("1C rollback left table %s: %v error=%v", table, found, err)
			}
		}
		if err := goose.DownContext(ctx, db, "."); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, "SELECT value FROM app_metadata WHERE key = 'schema_phase'").Scan(&phase); err != nil || phase != "1a" {
			t.Fatalf("after 1B rollback phase=%q error=%v", phase, err)
		}
		for _, table := range []string{"resources", "audit_events"} {
			var found sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT to_regclass($1)::text", schema+"."+table).Scan(&found); err != nil || found.Valid {
				t.Fatalf("1B rollback left table %s: %v error=%v", table, found, err)
			}
		}
		if err := goose.UpContext(ctx, db, "."); err != nil {
			t.Fatalf("reapply 1B and 1C: %v", err)
		}
		// DownTo zero verifies the full chain.
		if err := goose.DownToContext(ctx, db, ".", 0); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = $1 AND table_name <> 'goose_db_version'`, schema).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback left %d tables error=%v", count, err)
		}
	}
}
