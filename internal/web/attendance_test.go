package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
)

func TestAttendanceHTTPWorkflow(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	database := &store.Store{Pool: pool}
	ids := map[string]string{}
	cookies := map[string]*http.Cookie{}
	for _, name := range []string{"admin", "lecturer", "otherlecturer", "student", "student2", "outsider"} {
		role := name
		if name == "otherlecturer" {
			role = "lecturer"
		}
		if name == "student2" || name == "outsider" {
			role = "student"
		}
		var id string
		if err := pool.QueryRow(ctx, "INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$3,'unused-test-hash') RETURNING id::text", name+"@attendance.test", name, role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		token, err := auth.Token()
		if err != nil {
			t.Fatal(err)
		}
		if err := database.CreateSession(ctx, id, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		cookies[name] = &http.Cookie{Name: "cabv_session", Value: token}
	}
	var course, room string
	if err := pool.QueryRow(ctx, "INSERT INTO courses(code,title) VALUES('AT101','Attendance Test') RETURNING id::text").Scan(&course); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO rooms(name,kind,capacity) VALUES('Room T','room',50) RETURNING id::text").Scan(&room); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)", course, ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"student", "student2"} {
		if _, err := pool.Exec(ctx, "INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)", course, ids[name]); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler(pool.Ping, Options{Store: database})
	rawRequest := func(role, method, path, body, csrf, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if role != "" {
			r.AddCookie(cookies[role])
		}
		if method == "POST" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	api := func(role, method, path string, input any) *httptest.ResponseRecorder {
		var body []byte
		if method == "POST" {
			if input == nil {
				input = map[string]any{}
			}
			body, _ = json.Marshal(input)
		}
		csrf := ""
		if role != "" {
			csrf = auth.CSRF(cookies[role].Value)
		}
		return rawRequest(role, method, path, string(body), csrf, "")
	}
	decode := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("JSON %d: %s", w.Code, w.Body.String())
		}
		return result
	}
	create := map[string]any{"course_id": course, "room_id": room, "title": "Lecture 1", "duration_minutes": 30}
	for _, role := range []string{"student", "outsider", "otherlecturer"} {
		if w := api(role, "POST", "/api/v1/attendance/sessions", create); w.Code != 404 {
			t.Fatalf("%s create: %d", role, w.Code)
		}
	}
	if w := rawRequest("lecturer", "POST", "/api/v1/attendance/sessions", `{}`, "", ""); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := rawRequest("lecturer", "POST", "/api/v1/attendance/sessions", `{}`, auth.CSRF(cookies["lecturer"].Value), "https://evil.test"); w.Code != 403 {
		t.Fatal("cross-origin accepted")
	}
	w := api("lecturer", "POST", "/api/v1/attendance/sessions", create)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	created := decode(w)
	id := created["id"].(string)
	prefix := "/api/v1/attendance/sessions/" + id
	if created["status"] != "draft" || created["starts_at"] != nil || created["ends_at"] != nil {
		t.Fatal("invalid draft metadata")
	}
	if w := api("student", "POST", prefix+"/nonce", nil); w.Code != 409 {
		t.Fatalf("draft nonce: %d", w.Code)
	}
	if w := api("lecturer", "POST", prefix+"/challenge", nil); w.Code != 409 {
		t.Fatalf("draft challenge: %d", w.Code)
	}
	for _, body := range []string{"null", `{"decision":"present"}`, `{} {}`, `{"unknown":true}`} {
		if w := rawRequest("lecturer", "POST", prefix+"/open", body, auth.CSRF(cookies["lecturer"].Value), ""); w.Code != 400 {
			t.Fatalf("shape %s: %d", body, w.Code)
		}
	}
	if w := rawRequest("lecturer", "POST", prefix+"/open", `{"extra":"`+strings.Repeat("x", 5000)+`"}`, auth.CSRF(cookies["lecturer"].Value), ""); w.Code != 413 {
		t.Fatalf("body cap: %d", w.Code)
	}
	if w := api("lecturer", "POST", prefix+"/open", nil); w.Code != 200 {
		t.Fatalf("open: %d %s", w.Code, w.Body.String())
	}
	if w := api("lecturer", "POST", prefix+"/open", nil); w.Code != 409 {
		t.Fatal("reopened open session")
	}
	for _, role := range []string{"student", "student2", "outsider", "otherlecturer"} {
		if w := api(role, "POST", prefix+"/challenge", nil); w.Code != 404 {
			t.Fatalf("%s challenge access: %d", role, w.Code)
		}
		if w := api(role, "GET", prefix+"/results", nil); w.Code != 404 {
			t.Fatalf("%s roster: %d", role, w.Code)
		}
	}
	challenge := decode(api("lecturer", "POST", prefix+"/challenge", nil))["token"].(string)
	for _, path := range []string{prefix, prefix + "/mine", "/attendance/sessions/" + id, "/courses/" + course} {
		w := api("student", "GET", path, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), challenge) || strings.Contains(w.Body.String(), "student2@attendance.test") || strings.Contains(w.Body.String(), "token_hash") {
			t.Fatalf("student leak %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := api("student", "GET", prefix+"/challenge", nil); w.Code != 405 {
		t.Fatalf("challenge GET: %d", w.Code)
	}
	if w := api("lecturer", "POST", prefix+"/nonce", nil); w.Code != 404 {
		t.Fatal("lecturer received nonce")
	}
	nonce := decode(api("student", "POST", prefix+"/nonce", nil))["nonce"].(string)
	// Another student cannot consume or claim the owner's nonce.
	wrongOwner := decode(api("student2", "POST", prefix+"/verify", map[string]string{"token": challenge, "nonce": nonce}))
	if wrongOwner["status"] != "rejected" || wrongOwner["reason_code"] != "nonce_invalid" {
		t.Fatal("wrong owner accepted")
	}
	accepted := decode(api("student", "POST", prefix+"/verify", map[string]string{"token": challenge, "nonce": nonce}))
	if accepted["status"] != "pending" || accepted["reason_code"] != "pending_review" || accepted["duplicate"] != false {
		t.Fatalf("accepted=%v", accepted)
	}
	replay := decode(api("student", "POST", prefix+"/verify", map[string]string{"token": challenge, "nonce": nonce}))
	if replay["status"] != "rejected" || replay["reason_code"] != "nonce_used" {
		t.Fatalf("replay=%v", replay)
	}
	fresh := decode(api("student", "POST", prefix+"/nonce", nil))["nonce"].(string)
	duplicate := decode(api("student", "POST", prefix+"/verify", map[string]string{"token": challenge, "nonce": fresh}))
	if duplicate["duplicate"] != true || duplicate["status"] != "pending" {
		t.Fatalf("duplicate=%v", duplicate)
	}
	for _, role := range []string{"student", "otherlecturer"} {
		if w := api(role, "POST", prefix+"/reviews", map[string]string{"email": "student@attendance.test", "decision": "present", "reason": "Manual check"}); w.Code != 404 {
			t.Fatalf("unauthorized review: %d", w.Code)
		}
	}
	if w := api("lecturer", "POST", prefix+"/reviews", map[string]string{"email": "student@attendance.test", "decision": "present", "reason": ""}); w.Code != 400 {
		t.Fatal("blank manual reason accepted")
	}
	if w := api("lecturer", "POST", prefix+"/reviews", map[string]string{"email": "student@attendance.test", "decision": "verified", "reason": "Attempt to claim physical verification"}); w.Code != 400 {
		t.Fatal("unsupported verified status accepted")
	}
	if w := api("lecturer", "POST", prefix+"/reviews", map[string]string{"email": "student@attendance.test", "decision": "present", "reason": "Lecturer checked student in class"}); w.Code != 200 {
		t.Fatalf("manual review: %d %s", w.Code, w.Body.String())
	}
	mine := decode(api("student", "GET", prefix+"/mine", nil))
	if mine["status"] != "present" || mine["source"] != "manual" || mine["review_reason"] != "Lecturer checked student in class" {
		t.Fatalf("mine=%v", mine)
	}
	if w := api("lecturer", "GET", prefix+"/mine", nil); w.Code != 404 {
		t.Fatal("lecturer student result access")
	}
	// Rotation invalidates the former token, even with a fresh nonce.
	newer := decode(api("lecturer", "POST", prefix+"/challenge", nil))["token"].(string)
	fresh = decode(api("student", "POST", prefix+"/nonce", nil))["nonce"].(string)
	revoked := decode(api("student", "POST", prefix+"/verify", map[string]string{"token": challenge, "nonce": fresh}))
	if revoked["reason_code"] != "challenge_revoked" {
		t.Fatalf("revoked=%v", revoked)
	}
	fresh = decode(api("student", "POST", prefix+"/nonce", nil))["nonce"].(string)
	priorManual := decode(api("student", "POST", prefix+"/verify", map[string]string{"token": newer, "nonce": fresh}))
	if priorManual["duplicate"] != true || priorManual["status"] != "present" {
		t.Fatalf("manual overwritten: %v", priorManual)
	}
	var records int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM attendance_records WHERE session_id=$1 AND student_id=$2", id, ids["student"]).Scan(&records); err != nil || records != 1 {
		t.Fatalf("records=%d err=%v", records, err)
	}
	var audit string
	if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(metadata::text,' '),'') FROM audit_events").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, challenge) || strings.Contains(audit, newer) || strings.Contains(audit, nonce) {
		t.Fatal("audit contains plaintext secrets")
	}
	// Existing records remain visible to staff after current enrollment is withdrawn.
	if _, err := pool.Exec(ctx, "UPDATE enrollments SET status='withdrawn' WHERE course_id=$1 AND student_id=$2", course, ids["student"]); err != nil {
		t.Fatal(err)
	}
	if w := api("student", "GET", prefix+"/mine", nil); w.Code != 404 {
		t.Fatal("withdrawn student retained access")
	}
	results := decode(api("lecturer", "GET", prefix+"/results", nil))["results"].([]any)
	retained := false
	for _, value := range results {
		row := value.(map[string]any)
		if row["student_id"] == ids["student"] {
			retained = true
			if row["status"] != "present" || row["can_review"] != false {
				t.Fatalf("history=%v", row)
			}
		}
	}
	if !retained {
		t.Fatal("withdrawal hid historical record")
	}
	if w := api("lecturer", "POST", prefix+"/close", nil); w.Code != 200 {
		t.Fatal("close failed")
	}
	if w := api("lecturer", "POST", prefix+"/open", nil); w.Code != 409 {
		t.Fatal("closed session reopened")
	}
	if w := api("student2", "POST", prefix+"/nonce", nil); w.Code != 409 {
		t.Fatal("closed nonce issued")
	}
	if w := api("lecturer", "POST", prefix+"/challenge", nil); w.Code != 409 {
		t.Fatal("closed challenge issued")
	}
	if w := api("lecturer", "POST", prefix+"/reviews", map[string]string{"email": "student2@attendance.test", "decision": "rejected", "reason": "Reviewed after close"}); w.Code != 200 {
		t.Fatalf("closed review: %d", w.Code)
	}
	if w := api("", "GET", prefix, nil); w.Code != 401 {
		t.Fatal("anonymous API access")
	}
}

func TestAttendanceHTMLWorkflow(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	database := &store.Store{Pool: pool}
	var course, room, teacherID, studentID string
	for _, entry := range []struct {
		email, role string
		id          *string
	}{{"teacher@html.test", "lecturer", &teacherID}, {"student@html.test", "student", &studentID}} {
		if err := pool.QueryRow(ctx, "INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$2,'unused') RETURNING id::text", entry.email, entry.role).Scan(entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, "INSERT INTO courses(code,title) VALUES('HT101','HTML Attendance') RETURNING id::text").Scan(&course); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "INSERT INTO rooms(name,kind,capacity) VALUES('HTML Room','room',30) RETURNING id::text").Scan(&room); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO course_lecturers VALUES($1,$2)", course, teacherID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)", course, studentID); err != nil {
		t.Fatal(err)
	}
	teacherToken, _ := auth.Token()
	studentToken, _ := auth.Token()
	for _, entry := range []struct{ id, token string }{{teacherID, teacherToken}, {studentID, studentToken}} {
		if err := database.CreateSession(ctx, entry.id, auth.HashToken(entry.token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler(pool.Ping, Options{Store: database})
	request := func(token, method, path string, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.AddCookie(&http.Cookie{Name: "cabv_session", Value: token})
		if method == "POST" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	post := func(token, path string, form url.Values) *httptest.ResponseRecorder {
		form.Set("csrf", auth.CSRF(token))
		return request(token, "POST", path, form)
	}
	w := post(teacherToken, "/attendance/sessions", url.Values{"course_id": {course}, "room_id": {room}, "title": {"HTML lecture"}, "duration_minutes": {"30"}})
	if w.Code != 303 {
		t.Fatalf("HTMLcreate: %d %s", w.Code, w.Body.String())
	}
	path := w.Header().Get("Location")
	id := strings.TrimPrefix(path, "/attendance/sessions/")
	if w := request(teacherToken, "GET", path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Open attendance") {
		t.Fatalf("draft page: %d %s", w.Code, w.Body.String())
	}
	if w := post(teacherToken, path+"/open", url.Values{}); w.Code != 303 {
		t.Fatal("HTML open")
	}
	w = post(teacherToken, path+"/challenge", url.Values{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "challenge-qr") || !strings.Contains(w.Body.String(), "WAT") {
		t.Fatalf("challengepage: %d %s", w.Code, w.Body.String())
	}
	challenge, err := database.IssueAttendanceChallenge(ctx, store.User{ID: teacherID}, id)
	if err != nil {
		t.Fatal(err)
	}
	w = post(studentToken, path+"/nonce", url.Values{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Submit for lecturer review") || strings.Contains(w.Body.String(), challenge.Token) {
		t.Fatalf("noncepage: %d %s", w.Code, w.Body.String())
	}
	marker := `name="nonce" value="`
	start := strings.Index(w.Body.String(), marker)
	if start < 0 {
		t.Fatal("nonce formmissing")
	}
	nonce := w.Body.String()[start+len(marker):]
	nonce = strings.SplitN(nonce, `"`, 2)[0]
	w = post(studentToken, path+"/verify", url.Values{"token": {challenge.Token}, "nonce": {nonce}})
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("Pending lecturer review")) {
		t.Fatalf("pendingpage: %d %s", w.Code, w.Body.String())
	}
	w = post(teacherToken, path+"/reviews", url.Values{"email": {"student@html.test"}, "decision": {"present"}, "reason": {"Reviewed in person"}})
	if w.Code != 303 {
		t.Fatalf("reviewform: %d %s", w.Code, w.Body.String())
	}
	if w := request(studentToken, "GET", path, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Reviewed in person") || strings.Contains(w.Body.String(), "Issue a new challenge") {
		t.Fatalf("ownresult: %d %s", w.Code, w.Body.String())
	}
}
