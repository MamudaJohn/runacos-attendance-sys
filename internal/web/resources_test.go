package web

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/resources"
	"cabv/internal/store"
)

func resourceMultipart(t *testing.T, csrf, title, name string, data []byte, fileFirst, duplicate bool) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	writeFile := func() {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if fileFirst {
		writeFile()
	}
	if err := writer.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("title", title); err != nil {
		t.Fatal(err)
	}
	if !fileFirst {
		writeFile()
	}
	if duplicate {
		writeFile()
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func TestResourceHTTPPermissionsLimitsAndDownloads(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	s := &store.Store{Pool: pool}
	ids := map[string]string{}
	cookies := map[string]*http.Cookie{}
	for _, name := range []string{"student", "lecturer", "admin", "stranger"} {
		role := name
		if name == "stranger" {
			role = "student"
		}
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$1,$2,'test-hash') RETURNING id::text`, name+"@resource.example", role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		token, err := auth.Token()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, id, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		cookies[name] = &http.Cookie{Name: "cabv_session", Value: token}
	}
	var course, other string
	if err := pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('RES101','Resource course') RETURNING id::text`).Scan(&course); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('RES202','Other course') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO course_lecturers(course_id,lecturer_id) VALUES($1,$2)`, course, ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)`, course, ids["student"]); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files, err := resources.New(dir, resources.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(pool.Ping, Options{Store: s, Files: files})
	request := func(method, path string, body io.Reader, contentType string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, body)
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	post := func(actor, courseID, csrf, title, name string, data []byte, first, duplicate bool, headers map[string]string) *httptest.ResponseRecorder {
		body, typ := resourceMultipart(t, csrf, title, name, data, first, duplicate)
		return request("POST", "/courses/"+courseID+"/resources", bytes.NewReader(body), typ, cookies[actor], headers)
	}
	text := []byte("Lecture notes\nCourse resources only.\n")
	if w := post("lecturer", course, auth.CSRF(cookies["lecturer"].Value), "Lecture notes", `C:\private\notes.txt`, text, false, false, nil); w.Code != 303 || w.Header().Get("Location") != "/courses/"+course {
		t.Fatalf("upload=%d %s", w.Code, w.Body.String())
	}
	var resourceID, storageKey, originalName string
	if err := pool.QueryRow(ctx, `SELECT id::text,storage_key,original_name FROM resources WHERE course_id=$1`, course).Scan(&resourceID, &storageKey, &originalName); err != nil {
		t.Fatal(err)
	}
	if originalName != "notes.txt" {
		t.Fatalf("unsafe original filename=%q", originalName)
	}
	downloadPath := "/resources/" + resourceID + "/download"
	for _, actor := range []string{"student", "lecturer", "admin"} {
		w := request("GET", downloadPath, nil, "", cookies[actor], nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), text) || w.Header().Get("Content-Type") != "text/plain" || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s download=%d headers=%v body=%q", actor, w.Code, w.Header(), w.Body.String())
		}
		typ, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || typ != "attachment" || params["filename"] != "notes.txt" {
			t.Fatalf("disposition=%q error=%v", w.Header().Get("Content-Disposition"), err)
		}
	}
	if w := request("HEAD", downloadPath, nil, "", cookies["student"], nil); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("HEAD=%d %v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if w := request("GET", downloadPath, nil, "", cookies["student"], map[string]string{"Range": "bytes=2-7"}); w.Code != 206 || w.Body.String() != string(text[2:8]) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("range=%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if w := request("GET", downloadPath, nil, "", cookies["student"], map[string]string{"Range": "bytes=9000-9999"}); w.Code != 416 || w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("invalid range=%d headers=%v", w.Code, w.Header())
	}
	if w := request("GET", downloadPath, nil, "", nil, nil); w.Code != 303 {
		t.Fatalf("anonymous download=%d", w.Code)
	}
	for _, method := range []string{"GET", "HEAD"} {
		if w := request(method, downloadPath, nil, "", cookies["stranger"], nil); w.Code != 404 {
			t.Fatalf("stranger %s=%d", method, w.Code)
		}
	}
	if w := request("GET", "/courses/"+course, nil, "", cookies["student"], nil); w.Code != 200 || !strings.Contains(w.Body.String(), downloadPath) {
		t.Fatalf("student resource list=%d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, actor, courseID, csrf, title, fileName string
		data                                         []byte
		first, duplicate                             bool
		headers                                      map[string]string
		status                                       int
	}{
		{name: "student cannot upload", actor: "student", courseID: course, csrf: auth.CSRF(cookies["student"].Value), title: "Notes", fileName: "n.txt", data: text, status: 404},
		{name: "lecturer unrelated course", actor: "lecturer", courseID: other, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: text, status: 404},
		{name: "missing CSRF", actor: "lecturer", courseID: course, title: "Notes", fileName: "n.txt", data: text, status: 403},
		{name: "wrong CSRF", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["student"].Value), title: "Notes", fileName: "n.txt", data: text, status: 403},
		{name: "file before CSRF", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: text, first: true, status: 403},
		{name: "other origin", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: text, headers: map[string]string{"Origin": "http://evil.example"}, status: 403},
		{name: "HTML in txt", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: []byte("<html>bad</html>"), status: 415},
		{name: "binary txt", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: []byte{0, 1, 2}, status: 415},
		{name: "fake PDF", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.pdf", data: text, status: 415},
		{name: "unsupported extension", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.exe", data: text, status: 415},
		{name: "empty title", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), fileName: "n.txt", data: text, status: 400},
		{name: "large title", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: strings.Repeat("x", 201), fileName: "n.txt", data: text, status: 400},
		{name: "duplicate files cleanup", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: text, duplicate: true, status: 400},
		{name: "oversize file", actor: "lecturer", courseID: course, csrf: auth.CSRF(cookies["lecturer"].Value), title: "Notes", fileName: "n.txt", data: bytes.Repeat([]byte("x"), int(resources.MaxFileBytes)+1), status: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := post(tc.actor, tc.courseID, tc.csrf, tc.title, tc.fileName, tc.data, tc.first, tc.duplicate, tc.headers)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != storageKey {
		t.Fatalf("failed uploads retained files=%v error=%v", entries, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ADD CONSTRAINT reject_upload CHECK(action <> 'resource.uploaded') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if w := post("lecturer", course, auth.CSRF(cookies["lecturer"].Value), "Uncommitted", "uncommitted.txt", text, false, false, nil); w.Code != 503 {
		t.Fatalf("DBfailure=%d %s", w.Code, w.Body.String())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("DBfailure cleanup=%v error=%v", entries, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM resources`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("DBfailure resourcecount=%d error=%v", count, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DROP CONSTRAINT reject_upload`); err != nil {
		t.Fatal(err)
	}
	quotaFiles, err := resources.New(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	h = Handler(pool.Ping, Options{Store: s, Files: quotaFiles})
	if w := post("lecturer", course, auth.CSRF(cookies["lecturer"].Value), "Quota", "quota.txt", text, false, false, nil); w.Code != 507 {
		t.Fatalf("quota=%d %s", w.Code, w.Body.String())
	}
	h = Handler(pool.Ping, Options{Store: s, Files: files})
	if _, err := pool.Exec(ctx, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, ids["student"]); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		if w := request(method, downloadPath, nil, "", cookies["student"], map[string]string{"Range": "bytes=0-4"}); w.Code != 404 {
			t.Fatalf("withdrawn %s=%d", method, w.Code)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM course_lecturers WHERE lecturer_id=$1`, ids["lecturer"]); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", downloadPath, nil, "", cookies["lecturer"], nil); w.Code != 404 {
		t.Fatalf("reassigned download=%d", w.Code)
	}
	if w := post("lecturer", course, auth.CSRF(cookies["lecturer"].Value), "Revoked", "revoked.txt", text, false, false, nil); w.Code != 404 {
		t.Fatalf("reassigned upload=%d", w.Code)
	}
}

func TestResourceUploadRejectsBeforeReadingAndBoundsConcurrency(t *testing.T) {
	pool := platformDB(t)
	ctx := context.Background()
	s := &store.Store{Pool: pool}
	var id, course string
	if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES('admin@early.example','Admin','admin','hash') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('EAR101','Early authorization') RETURNING id::text`).Scan(&course); err != nil {
		t.Fatal(err)
	}
	token, err := auth.Token()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, id, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	files, err := resources.New(t.TempDir(), resources.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	a := &application{Options: Options{Store: s}, resources: files, uploadSlots: make(chan struct{}, 2)}
	for _, tc := range []struct {
		name, origin string
		busy         bool
		status       int
	}{{"origin", "http://evil.example", false, 403}, {"busy", "", true, 429}} {
		t.Run(tc.name, func(t *testing.T) {
			body := &countReads{}
			r := httptest.NewRequest("POST", "/courses/"+course+"/resources", body)
			r.SetPathValue("id", course)
			r.Header.Set("Origin", tc.origin)
			r.AddCookie(&http.Cookie{Name: "cabv_session", Value: token})
			if tc.busy {
				a.uploadSlots <- struct{}{}
				a.uploadSlots <- struct{}{}
				defer func() { <-a.uploadSlots; <-a.uploadSlots }()
			}
			w := httptest.NewRecorder()
			a.uploadResource(w, r)
			if w.Code != tc.status || body.reads != 0 {
				t.Fatalf("status=%d bodyreads=%d", w.Code, body.reads)
			}
		})
	}
}

type countReads struct{ reads int }

func (r *countReads) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }
