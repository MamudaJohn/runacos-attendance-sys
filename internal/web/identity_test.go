package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/identity"
	"cabv/internal/store"
)

func TestIdentityRoutesOriginCSRFPasswordAndPrivacy(t *testing.T) {
	pool, ctx := platformDB(t), context.Background()
	s := &store.Store{Pool: pool}
	const password = "Identity-test-password-2026!"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	tokens, users := map[string]string{}, map[string]store.User{}
	for _, role := range []string{"student", "lecturer", "admin"} {
		var user store.User
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$2,$3) RETURNING id::text,email,name,role`, role+"@identity.example", role, hash).Scan(&user.ID, &user.Email, &user.Name, &user.Role); err != nil {
			t.Fatal(err)
		}
		users[role] = user
		token, err := auth.Token()
		if err != nil {
			t.Fatal(err)
		}
		tokens[role] = token
		if err := s.CreateSession(ctx, user.ID, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	service, err := identity.New(s, "attendance.test", "https://attendance.test")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(pool.Ping, Options{Store: s, SecureCookies: true, Identity: service})
	request := func(method, path, body, role, origin, csrf, contentType, fetchSite string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://attendance.test"+path, strings.NewReader(body))
		if token := tokens[role]; token != "" {
			r.AddCookie(&http.Cookie{Name: "__Host-cabv_session", Value: token})
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if csrf == "valid" {
			csrf = auth.CSRF(tokens[role])
		}
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Content-Type", contentType)
		if fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		// Forwarded headers must never relax the configured exact origin.
		r.Header.Set("X-Forwarded-Host", "attacker.test")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	registration := `{"password":"` + password + `"}`
	for _, tc := range []struct {
		name, body, role, origin, csrf, contentType, site string
		want                                              int
	}{
		{"no login", registration, "", "https://attendance.test", "valid", "application/json", "", 401},
		{"other origin", registration, "student", "https://attacker.test", "valid", "application/json", "", 403},
		{"missing origin", registration, "student", "", "valid", "application/json", "", 403},
		{"missing csrf", registration, "student", "https://attendance.test", "", "application/json", "", 403},
		{"another account csrf", registration, "student", "https://attendance.test", auth.CSRF(tokens["admin"]), "application/json", "", 403},
		{"cross site fetch", registration, "student", "https://attendance.test", "valid", "application/json", "cross-site", 403},
		{"wrong content type", registration, "student", "https://attendance.test", "valid", "text/plain", "", 415},
		{"unknown field", `{"password":"test","verified":true}`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"json null", `null`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"json array", `[]`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"trailing json", registration + `{}`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"malformed json", `{"password":`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"wrong field type", `{"password":true}`, "student", "https://attendance.test", "valid", "application/json", "", 400},
		{"large body", `{"password":"` + strings.Repeat("a", 21*1024) + `"}`, "student", "https://attendance.test", "valid", "application/json", "", 413},
		{"lecturer", registration, "lecturer", "https://attendance.test", "valid", "application/json", "", 403},
		{"administrator", registration, "admin", "https://attendance.test", "valid", "application/json", "", 403},
		{"wrong password", `{"password":"wrong-password"}`, "student", "https://attendance.test", "valid", "application/json", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request("POST", "/api/v1/identity/register/options", tc.body, tc.role, tc.origin, tc.csrf, tc.contentType, tc.site)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_ceremonies`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid requests created %d ceremonies", count)
	}
	w := request("POST", "/api/v1/identity/register/options", registration, "student", "https://attendance.test", "valid", "application/json", "same-origin")
	if w.Code != 200 {
		t.Fatalf("valid options=%d %s", w.Code, w.Body.String())
	}
	var options struct {
		TransactionID string `json:"transaction_id"`
		PublicKey     struct {
			Challenge              string `json:"challenge"`
			AuthenticatorSelection struct {
				UV string `json:"userVerification"`
			} `json:"authenticatorSelection"`
		} `json:"public_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if options.TransactionID == "" || options.PublicKey.Challenge == "" || options.PublicKey.AuthenticatorSelection.UV != "required" {
		t.Fatalf("options=%+v", options)
	}
	w = request("POST", "/api/v1/identity/register/finish", `{"transaction_id":"`+options.TransactionID+`","credential":{"verified":true}}`, "student", "https://attendance.test", "valid", "application/json", "")
	if w.Code != 401 {
		t.Fatalf("client biometric boolean accepted: %d %s", w.Code, w.Body.String())
	}
	var used bool
	if err := pool.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM identity_ceremonies WHERE id=$1`, options.TransactionID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("failed finish did not consume ceremony")
	}
	w = request("GET", "/api/v1/identity", "", "student", "", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":true`) || !strings.Contains(w.Body.String(), "fingerprint, face or PIN") {
		t.Fatalf("identity notice=%d %s", w.Code, w.Body.String())
	}
	for _, private := range []string{"credential_data", "publicKey", "password_hash", "biometricTemplate"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("identity status exposed %s", private)
		}
	}
	if got := request("GET", "/api/v1/identity", "", "lecturer", "", "", "", ""); got.Code != 403 {
		t.Fatalf("lecturer status=%d", got.Code)
	}
}

func TestIdentityDisabledAndRecoveryAuthorization(t *testing.T) {
	pool, ctx := platformDB(t), context.Background()
	s := &store.Store{Pool: pool}
	const password = "Identity-recovery-password-2026!"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	var student, admin store.User
	for _, entry := range []struct {
		u    *store.User
		role string
	}{{&student, "student"}, {&admin, "admin"}} {
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$2,$3) RETURNING id::text,email,name,role`, entry.role+"@recovery.example", entry.role, hash).Scan(&entry.u.ID, &entry.u.Email, &entry.u.Name, &entry.u.Role); err != nil {
			t.Fatal(err)
		}
	}
	token, err := auth.Token()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, student.ID, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	request := func(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://attendance.test"+path, strings.NewReader(body))
		r.Header.Set("Origin", "https://attendance.test")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", auth.CSRF(token))
		r.AddCookie(&http.Cookie{Name: "__Host-cabv_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	disabled := Handler(pool.Ping, Options{Store: s, SecureCookies: true})
	if w := request(disabled, "GET", "/api/v1/identity", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("disabled status=%d %s", w.Code, w.Body.String())
	}
	if w := request(disabled, "POST", "/api/v1/identity/register/options", `{}`); w.Code != 503 {
		t.Fatalf("disabled identity=%d", w.Code)
	}
	service, err := identity.New(s, "attendance.test", "https://attendance.test")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(pool.Ping, Options{Store: s, SecureCookies: true, Identity: service})
	if w := request(h, "POST", "/api/v1/admin/users/"+student.ID+"/identity/reset", `{"password":"`+password+`","reason":"lost phone"}`); w.Code != 403 {
		t.Fatalf("student reset=%d %s", w.Code, w.Body.String())
	}
	// Session authorization is re-read on every request; deactivation immediately
	// stops options, status and recovery even when the cookie is still valid.
	if _, err := pool.Exec(ctx, `UPDATE users SET active=false WHERE id=$1`, student.ID); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", "/api/v1/identity/register/options", `{"password":"`+password+`"}`); w.Code != 401 {
		t.Fatalf("inactive student options=%d", w.Code)
	}
}

func TestIdentityRequiredCannotBeDisabledByAttendanceClient(t *testing.T) {
	pool, ctx := platformDB(t), context.Background()
	s := &store.Store{Pool: pool}
	var student, admin store.User
	for _, entry := range []struct {
		user *store.User
		role string
	}{{&student, "student"}, {&admin, "admin"}} {
		if err := pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$2,$2,'test-hash') RETURNING id::text,email,name,role`, entry.role+"@identity-required.example", entry.role).Scan(&entry.user.ID, &entry.user.Email, &entry.user.Name, &entry.user.Role); err != nil {
			t.Fatal(err)
		}
	}
	var course string
	if err := pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('ID201','Required identity') RETURNING id::text`).Scan(&course); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2)`, course, student.ID); err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, admin, "", "Identity required room", "room", 30)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAttendanceSession(ctx, admin, course, room.ID, "Required identity lecture", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAttendanceSession(ctx, admin, session.ID); err != nil {
		t.Fatal(err)
	}
	challenge, err := s.IssueAttendanceChallenge(ctx, admin, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.Token()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, student.ID, auth.HashToken(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	service, err := identity.New(s, "attendance.test", "https://attendance.test")
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(pool.Ping, Options{Store: s, SecureCookies: true, Identity: service})
	request := func(path, kind, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://attendance.test"+path, strings.NewReader(body))
		r.Header.Set("Origin", "https://attendance.test")
		r.Header.Set("Content-Type", kind)
		r.Header.Set("X-CSRF-Token", auth.CSRF(token))
		r.AddCookie(&http.Cookie{Name: "__Host-cabv_session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	apiPath := "/api/v1/attendance/sessions/" + session.ID + "/verify"
	for _, field := range []string{"require_identity", "RequireIdentity", "identity_enabled", "biometric_verified"} {
		t.Run("reject client policy field "+field, func(t *testing.T) {
			nonce, err := s.Nonce(ctx, student, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"token": challenge.Token, "nonce": nonce.Nonce, field: false})
			if err != nil {
				t.Fatal(err)
			}
			w := request(apiPath, "application/json", string(body))
			if w.Code != 400 {
				t.Fatalf("client policy accepted: %d %s", w.Code, w.Body.String())
			}
			var attempts int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM verification_attempts`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 {
				t.Fatal("unexpected policy field reached verifier")
			}
		})
	}
	// Older clients only submit token and nonce. Server configuration still
	// requires a cryptographic identity proof, independent of client UI state.
	nonce, err := s.Nonce(ctx, student, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"token": challenge.Token, "nonce": nonce.Nonce})
	if err != nil {
		t.Fatal(err)
	}
	w := request(apiPath, "application/json", string(body))
	if w.Code != 200 {
		t.Fatalf("legacy client=%d %s", w.Code, w.Body.String())
	}
	var decision struct {
		Status string `json:"status"`
		Reason string `json:"reason_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Status != "rejected" || decision.Reason != "identity_required" {
		t.Fatalf("legacy client bypassed identity: %+v", decision)
	}
	// A modified DOM can add hidden controls to an ordinary HTML form. They
	// must not change the server's policy when JavaScript is absent or edited.
	nonce, err = s.Nonce(ctx, student, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {auth.CSRF(token)}, "token": {challenge.Token}, "nonce": {nonce.Nonce}, "require_identity": {"false"}, "RequireIdentity": {"false"}, "identity_enabled": {"false"}, "biometric_verified": {"true"}}
	w = request("/attendance/sessions/"+session.ID+"/verify", "application/x-www-form-urlencoded", form.Encode())
	if w.Code != 200 || !strings.Contains(w.Body.String(), attendanceReasonLabel("identity_required")) {
		t.Fatalf("HTML client bypassed identity: %d %s", w.Code, w.Body.String())
	}
	var attempts, records, identityRejected int
	if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE reason_code='identity_required' AND decision='rejected' AND identity_proof_id IS NULL) FROM verification_attempts`).Scan(&attempts, &identityRejected); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || identityRejected != 2 || records != 0 {
		t.Fatalf("attempts=%d identity rejected=%d attendance records=%d", attempts, identityRejected, records)
	}
}
