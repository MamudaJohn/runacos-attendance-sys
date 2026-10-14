package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cabv/internal/auth"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func seedTestPool(t *testing.T) *pgxpool.Pool {
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
	schema := "cabv_seed_test_" + hex.EncodeToString(suffix[:])
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
	return pool
}

func TestDevelopmentSeedIsIdempotentAndPreservesPasswords(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	password := "initial-demo-password"
	if err := seed(ctx, pool, password); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE email='student@cabv.test'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if !auth.VerifyPassword(before, password) {
		t.Fatal("seed did not store a valid password hash")
	}
	if err := seed(ctx, pool, "changed-demo-password"); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM users WHERE email='student@cabv.test'").Scan(&after); err != nil || before != after {
		t.Fatalf("reseed changed existing password error=%v", err)
	}
	for _, tc := range []struct {
		table string
		want  int
	}{
		{"users", 3}, {"courses", 2}, {"course_lecturers", 1}, {"enrollments", 1}, {"rooms", 1},
	} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{tc.table}.Sanitize()).Scan(&count); err != nil || count != tc.want {
			t.Fatalf("%s count=%d want=%d error=%v", tc.table, count, tc.want, err)
		}
	}
	var courseCode string
	if err := pool.QueryRow(ctx, "SELECT c.code FROM enrollments e JOIN courses c ON c.id=e.course_id").Scan(&courseCode); err != nil || courseCode != "CSC101" {
		t.Fatalf("demo enrollment course=%q error=%v", courseCode, err)
	}
}

func TestDevelopmentSeedRollsBackConflictingAccount(t *testing.T) {
	pool := seedTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO users(email,name,password_hash,role,active)
		VALUES('student@cabv.test','Existing student','existing-hash','student',false)`); err != nil {
		t.Fatal(err)
	}
	if err := seed(ctx, pool, "initial-demo-password"); err == nil {
		t.Fatal("disabled existing account should abort seed")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil || count != 1 {
		t.Fatalf("seed left partial accounts count=%d error=%v", count, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM courses").Scan(&count); err != nil || count != 0 {
		t.Fatalf("seed left partial courses count=%d error=%v", count, err)
	}
}
