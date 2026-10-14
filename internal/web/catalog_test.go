package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
)

func TestCatalogAdministration(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	database := &store.Store{Pool: pool}
	cookies := map[string]*http.Cookie{}
	ids := map[string]string{}
	for _, role := range []string{"admin", "student", "lecturer"} {
		var id string
		if err := pool.QueryRow(ctx, "INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$3,'unused-test-hash') RETURNING id::text", role+"@catalog.test", role, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[role] = id
		token, err := auth.Token()
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CreateSession(ctx, id, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		cookies[role] = &http.Cookie{Name: "cabv_session", Value: token}
	}
	h := Handler(pool.Ping, Options{Store: database})
	request := func(role, method, path string, values url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
		r.AddCookie(cookies[role])
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	post := func(role, path string, values url.Values) *httptest.ResponseRecorder {
		values.Set("csrf", auth.CSRF(cookies[role].Value))
		return request(role, "POST", path, values)
	}
	for _, role := range []string{"student", "lecturer"} {
		if w := request(role, "GET", "/admin/catalog", nil); w.Code != 403 {
			t.Fatalf("%s catalog: %d", role, w.Code)
		}
		for _, path := range []string{"/admin/courses", "/admin/rooms", "/admin/courses/no-id/lecturers", "/admin/courses/no-id/enrollments"} {
			if w := post(role, path, url.Values{}); w.Code != 403 {
				t.Fatalf("%s mutation %s: %d", role, path, w.Code)
			}
		}
	}
	if w := request("admin", "POST", "/admin/courses", url.Values{"code": {"TEST1"}, "title": {"Test course"}}); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	w := post("admin", "/admin/courses", url.Values{"code": {"test1"}, "title": {"Test course"}})
	if w.Code != 303 {
		t.Fatalf("create course: %d %s", w.Code, w.Body.String())
	}
	id := strings.TrimSuffix(strings.TrimPrefix(w.Header().Get("Location"), "/admin/courses/"), "?saved=1")
	if w := post("admin", "/admin/courses", url.Values{"code": {"TEST1"}, "title": {"Duplicate"}}); w.Code != 409 {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if w := post("admin", "/admin/courses", url.Values{"id": {id}, "code": {"TEST1"}, "title": {"<script>alert(1)</script>"}}); w.Code != 303 {
		t.Fatalf("edit: %d", w.Code)
	}
	if w := request("admin", "GET", "/admin/courses/"+id, nil); w.Code != 200 || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("unsafe/failed edit page: %d", w.Code)
	}
	if w := post("admin", "/admin/rooms", url.Values{"name": {"Theatre B"}, "kind": {"theatre"}, "capacity": {"1200"}}); w.Code != 303 {
		t.Fatalf("create room: %d %s", w.Code, w.Body.String())
	}
	var roomID string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM rooms WHERE name='Theatre B'").Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	if w := post("admin", "/admin/rooms", url.Values{"id": {roomID}, "name": {"Room B"}, "kind": {"room"}, "capacity": {"60"}}); w.Code != 303 {
		t.Fatalf("edit room: %d", w.Code)
	}
	for _, capacity := range []string{"0", "10001", "garbage"} {
		if w := post("admin", "/admin/rooms", url.Values{"name": {"Invalid room"}, "kind": {"room"}, "capacity": {capacity}}); w.Code != 400 {
			t.Fatalf("invalid capacity: %d", w.Code)
		}
	}
	if w := post("admin", "/admin/courses/"+id+"/lecturers", url.Values{"email": {"student@catalog.test"}, "assignment": {"assigned"}}); w.Code != 404 {
		t.Fatalf("wrong lecturer role: %d", w.Code)
	}
	if w := post("admin", "/admin/courses/"+id+"/enrollments", url.Values{"email": {"lecturer@catalog.test"}, "status": {"active"}}); w.Code != 404 {
		t.Fatalf("wrong student role: %d", w.Code)
	}
	if w := post("admin", "/admin/courses/"+id+"/lecturers", url.Values{"email": {"lecturer@catalog.test"}, "assignment": {"assigned"}}); w.Code != 303 {
		t.Fatalf("assign: %d %s", w.Code, w.Body.String())
	}
	if w := post("admin", "/admin/courses/"+id+"/enrollments", url.Values{"email": {"student@catalog.test"}, "status": {"active"}}); w.Code != 303 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body.String())
	}
	for _, role := range []string{"student", "lecturer"} {
		if w := request(role, "GET", "/courses/"+id, nil); w.Code != 200 {
			t.Fatalf("course access: %d %s", w.Code, w.Body.String())
		}
		if w := request(role, "GET", "/admin/courses/"+id, nil); w.Code != 403 {
			t.Fatalf("roster exposure: %d", w.Code)
		}
	}
	if w := request("admin", "GET", "/admin/courses/"+id, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "student@catalog.test") || !strings.Contains(w.Body.String(), "lecturer@catalog.test") {
		t.Fatalf("roster: %d %s", w.Code, w.Body.String())
	}
	if w := post("admin", "/admin/courses/"+id+"/enrollments", url.Values{"email": {"student@catalog.test"}, "status": {"withdrawn"}}); w.Code != 303 {
		t.Fatal("withdraw failed")
	}
	if w := request("student", "GET", "/courses/"+id, nil); w.Code != 404 {
		t.Fatal("withdrawal retained access")
	}
	if w := post("admin", "/admin/courses/"+id+"/lecturers", url.Values{"email": {"lecturer@catalog.test"}, "assignment": {"removed"}}); w.Code != 303 {
		t.Fatal("remove assignment failed")
	}
	if w := request("lecturer", "GET", "/courses/"+id, nil); w.Code != 404 {
		t.Fatal("assignment removal retained access")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE actor_id=$1", ids["admin"]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 8 {
		t.Fatalf("audit count=%d, want8", count)
	}
	if w := request("admin", "GET", "/admin/catalog", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Room B") || !strings.Contains(w.Body.String(), "TEST1") {
		t.Fatalf("catalog: %d", w.Code)
	}
}
