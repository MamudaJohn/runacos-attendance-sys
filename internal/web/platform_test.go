package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func platformDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.Token()
	if err != nil {
		t.Fatal(err)
	}
	schema := "web_test_" + auth.HashToken(token)[:16]
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var db *sql.DB = stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPlatformSecurity(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	password := "Test-password-2026!"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, role := range []string{"student", "lecturer", "admin"} {
		var id string
		if err := pool.QueryRow(ctx, "INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$3,$4) RETURNING id::text", role+"@test.example", role, role, hash).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[role] = id
	}
	var assigned, other string
	if err := pool.QueryRow(ctx, "INSERT INTO courses(code,title) VALUES('CSC101','Assigned course') RETURNING id::text").Scan(&assigned); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO courses(code,title) VALUES('CSC202','Private course') RETURNING id::text").Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)", assigned, ids["student"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)", assigned, ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	h := Handler(pool.Ping, Options{Store: &store.Store{Pool: pool}})
	requestNumber := 0
	request := func(method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		requestNumber++
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:4321", requestNumber)
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	findCookie := func(w *httptest.ResponseRecorder, name string) *http.Cookie {
		t.Helper()
		for _, cookie := range w.Result().Cookies() {
			if cookie.Name == name {
				return cookie
			}
		}
		t.Fatalf("missing cookie %s", name)
		return nil
	}
	login := func(role string, old ...*http.Cookie) *http.Cookie {
		t.Helper()
		page := request("GET", "/login", nil)
		csrf := findCookie(page, "cabv_login")
		if !strings.Contains(page.Body.String(), auth.CSRF(csrf.Value)) {
			t.Fatal("form missing CSRF")
		}
		cookies := append([]*http.Cookie{csrf}, old...)
		w := request("POST", "/auth/login", url.Values{"email": {role + "@test.example"}, "password": {password}, "csrf": {auth.CSRF(csrf.Value)}}, cookies...)
		if w.Code != 303 || w.Header().Get("Location") != "/dashboard" {
			t.Fatalf("login %s: %d %s", role, w.Code, w.Body.String())
		}
		session := findCookie(w, "cabv_session")
		if !session.HttpOnly || session.Path != "/" || session.SameSite != http.SameSiteLaxMode || session.MaxAge != 43200 {
			t.Fatal("bad session cookie options")
		}
		return session
	}
	if w := request("GET", "/dashboard", nil); w.Code != 303 {
		t.Fatalf("anonymous dashboard: %d", w.Code)
	}
	if w := request("GET", "/api/v1/courses", nil); w.Code != 401 {
		t.Fatalf("anonymous API: %d", w.Code)
	}
	student := login("student")
	lecturer := login("lecturer")
	admin := login("admin")
	for _, session := range []*http.Cookie{student, lecturer} {
		if w := request("GET", "/dashboard", nil, session); w.Code != 200 || strings.Contains(w.Body.String(), "CSC202") || !strings.Contains(w.Body.String(), "CSC101") {
			t.Fatalf("dashboard scope: %d %s", w.Code, w.Body.String())
		}
		if w := request("GET", "/courses/"+assigned, nil, session); w.Code != 200 {
			t.Fatalf("assigned: %d", w.Code)
		}
		for _, id := range []string{other, "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
			if w := request("GET", "/courses/"+id, nil, session); w.Code != 404 {
				t.Fatalf("private or absent: %d", w.Code)
			}
		}
		if w := request("GET", "/admin/users", nil, session); w.Code != 403 {
			t.Fatal("non-admin accessed account form")
		}
		if w := request("POST", "/admin/users", url.Values{"csrf": {auth.CSRF(session.Value)}}, session); w.Code != 403 {
			t.Fatal("non-admin account POST")
		}
	}
	if w := request("GET", "/courses/"+other, nil, admin); w.Code != 200 {
		t.Fatal("admin course access")
	}
	accountForm := url.Values{"name": {"New Student"}, "email": {"new@test.example"}, "role": {"student"}, "password": {password}, "csrf": {auth.CSRF(admin.Value)}}
	if w := request("POST", "/admin/users", accountForm, admin); w.Code != 201 {
		t.Fatalf("admin creation: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", "/admin/users", accountForm, admin); w.Code != 409 {
		t.Fatalf("duplicate account: %d", w.Code)
	}
	for _, csrf := range []string{"", auth.CSRF(lecturer.Value)} {
		if w := request("POST", "/auth/logout", url.Values{"csrf": {csrf}}, student); w.Code != 403 {
			t.Fatalf("invalid logout: %d", w.Code)
		}
	}
	if w := request("GET", "/dashboard", nil, student); w.Code != 200 {
		t.Fatal("invalid logout revoked session")
	}
	page := request("GET", "/login", nil)
	csrf := findCookie(page, "cabv_login")
	for _, submitted := range []string{"", auth.CSRF(student.Value)} {
		if w := request("POST", "/auth/login", url.Values{"email": {"student@test.example"}, "password": {password}, "csrf": {submitted}}, csrf); w.Code != 403 {
			t.Fatalf("login CSRF: %d", w.Code)
		}
	}
	for _, email := range []string{"student@test.example", "absent@test.example"} {
		w := request("POST", "/auth/login", url.Values{"email": {email}, "password": {"wrong password"}, "csrf": {auth.CSRF(csrf.Value)}}, csrf)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "Email or password is incorrect.") {
			t.Fatalf("credential failure: %d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(url.Values{"csrf": {auth.CSRF(csrf.Value)}, "password": {strings.Repeat("x", 9000)}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(csrf)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversized: %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/auth/logout", strings.NewReader(url.Values{"csrf": {auth.CSRF(student.Value)}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(student)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("origin: %d", w.Code)
	}
	rotated := login("student", student)
	if rotated.Value == student.Value {
		t.Fatal("token not rotated")
	}
	if w := request("GET", "/dashboard", nil, student); w.Code != 303 {
		t.Fatal("old cookie survived login rotation")
	}
	if w := request("POST", "/auth/logout", url.Values{"csrf": {auth.CSRF(rotated.Value)}}, rotated); w.Code != 303 {
		t.Fatal("logout failed")
	}
	if w := request("GET", "/dashboard", nil, rotated); w.Code != 303 {
		t.Fatal("copied cookie survived logout")
	}
	student = login("student")
	if _, err := pool.Exec(ctx, "UPDATE enrollments SET status='withdrawn' WHERE student_id=$1", ids["student"]); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/courses/"+assigned, nil, student); w.Code != 404 {
		t.Fatal("withdrawn enrollment retained access")
	}
	if _, err := pool.Exec(ctx, "UPDATE users SET active=false WHERE id=$1", ids["student"]); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/dashboard", nil, student); w.Code != 303 {
		t.Fatal("inactive account retained session")
	}
	if _, err := pool.Exec(ctx, "UPDATE sessions SET expires_at=now()-interval '1 second' WHERE user_id=$1", ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/dashboard", nil, lecturer); w.Code != 303 {
		t.Fatal("expired cookie accepted")
	}
	// Account quota remains effective when a client spoofs forwarding headers or changes IP.
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(url.Values{"email": {"rate@test.example"}, "password": {"wrong password"}, "csrf": {auth.CSRF(csrf.Value)}}.Encode()))
		r.RemoteAddr = fmt.Sprintf("198.51.100.%d:4321", i)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		r.AddCookie(csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expected := 401
		if i == 10 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
}

func TestSecureLoginCookie(t *testing.T) {
	h := Handler(func(context.Context) error { return nil }, Options{SecureCookies: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://cabv.example/login", nil))
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || cookies[0].Name != "__Host-cabv_login" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Domain != "" || cookies[0].Path != "/" {
		t.Fatalf("secure cookies=%+v status=%d", cookies, w.Code)
	}
}

func TestLoginOriginAndFetchSite(t *testing.T) {
	a := &application{Options: Options{SecureCookies: true}}
	for _, tc := range []struct {
		origin, site string
		allowed      bool
	}{{"https://cabv.example", "same-origin", true}, {"https://evil.example", "cross-site", false}, {"https://cabv.example", "same-site", false}, {"http://cabv.example", "", false}, {"", "", true}} {
		r := httptest.NewRequest("POST", "https://cabv.example/auth/logout", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		if a.sameOrigin(r) != tc.allowed {
			t.Fatalf("origin=%q site=%q", tc.origin, tc.site)
		}
	}
}
