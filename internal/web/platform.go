package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type view struct {
	Title, Error, Message, CSRF string
	User                        *store.User
	Courses                     []store.Course
	Course                      *store.Course
	Rooms                       []store.Room
	Members                     []store.Member
	Resources                   []store.Resource
	CanUpload                   bool
	AttendanceSessions          []store.AttendanceSession
	AttendanceSession           *store.AttendanceSession
	AttendanceResults           []store.AttendanceResult
	MyAttendance                *store.AttendanceResult
	Challenge                   *store.AttendanceChallenge
	AttendanceNonce             *store.AttendanceNonce
	AttendanceDecision          *store.AttendanceDecision
	CanManageAttendance         bool
	CanCheckIn                  bool
}

func (a *application) render(w http.ResponseWriter, code int, name string, data view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = a.pages.ExecuteTemplate(w, name, data)
}

func (a *application) cookieName(base string) string {
	if a.SecureCookies {
		return "__Host-" + base
	}
	return base
}
func (a *application) setCookie(w http.ResponseWriter, base, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(base), Value: value, Path: "/", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
func (a *application) cookie(r *http.Request, base string) string {
	c, err := r.Cookie(a.cookieName(base))
	if err != nil || len(c.Value) != 43 {
		return ""
	}
	return c.Value
}

func (a *application) loginPage(w http.ResponseWriter, r *http.Request) {
	token, err := auth.Token()
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	a.setCookie(w, "cabv_login", token, 600)
	a.render(w, 200, "login.html", view{Title: "Sign in", CSRF: auth.CSRF(token)})
}

// Browser mutations must come from the same origin. Forwarded headers are not trusted.
func (a *application) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		scheme := "http"
		if a.SecureCookies {
			scheme = "https"
		}
		return origin == scheme+"://"+r.Host
	}
	return true
}

func (a *application) form(w http.ResponseWriter, r *http.Request, token string) bool {
	if !a.sameOrigin(r) {
		http.Error(w, "Invalid request origin", 403)
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		http.Error(w, "Expected form submission", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form is too large or invalid", 413)
		return false
	}
	if token == "" || !auth.ValidCSRF(token, r.PostForm.Get("csrf")) {
		http.Error(w, "Invalid form token; reload the page", 403)
		return false
	}
	return true
}

func (a *application) login(w http.ResponseWriter, r *http.Request) {
	if a.Store == nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !a.limiter.Allow(ip) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "Too many attempts; try again later", 429)
		return
	}
	csrf := a.cookie(r, "cabv_login")
	if !a.form(w, r, csrf) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	password := r.PostForm.Get("password")
	if len(email) > 254 || len(password) > 128 {
		a.loginError(w, csrf)
		return
	}
	if !a.accountLimiter.Allow(auth.HashToken(email)) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "Too many attempts; try again later", 429)
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		http.Error(w, "Please try again shortly", 429)
		return
	}
	user, hash, err := a.Store.UserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		hash = a.dummyHash
	}
	verified := auth.VerifyPassword(hash, password)
	if !verified || err != nil {
		a.loginError(w, csrf)
		return
	}
	token, err := auth.Token()
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	if previous := a.cookie(r, "cabv_session"); previous != "" {
		if err := a.Store.DeleteSession(r.Context(), auth.HashToken(previous)); err != nil {
			http.Error(w, "Temporarily unavailable", 503)
			return
		}
	}
	if err := a.Store.CreateSession(r.Context(), user.ID, auth.HashToken(token), time.Now().Add(12*time.Hour)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			a.loginError(w, csrf)
			return
		}
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	a.setCookie(w, "cabv_session", token, 12*60*60)
	a.setCookie(w, "cabv_login", "", -1)
	http.Redirect(w, r, "/dashboard", 303)
}
func (a *application) loginError(w http.ResponseWriter, csrf string) {
	a.render(w, 401, "login.html", view{Title: "Sign in", CSRF: auth.CSRF(csrf), Error: "Email or password is incorrect."})
}

func (a *application) current(w http.ResponseWriter, r *http.Request) (store.User, string, bool) {
	token := a.cookie(r, "cabv_session")
	if a.Store == nil {
		http.Error(w, "Temporarily unavailable", 503)
		return store.User{}, "", false
	}
	if token != "" {
		user, err := a.Store.Session(r.Context(), auth.HashToken(token))
		if err == nil {
			return user, token, true
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "Temporarily unavailable", 503)
			return store.User{}, "", false
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.Error(w, "Authentication required", 401)
	} else {
		http.Redirect(w, r, "/login", 303)
	}
	return store.User{}, "", false
}

func (a *application) logout(w http.ResponseWriter, r *http.Request) {
	_, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	if err := a.Store.DeleteSession(r.Context(), auth.HashToken(token)); err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	a.setCookie(w, "cabv_session", "", -1)
	http.Redirect(w, r, "/login", 303)
}

func (a *application) dashboard(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	courses, err := a.Store.Courses(r.Context(), user)
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	a.render(w, 200, "dashboard.html", view{Title: "Your courses", User: &user, CSRF: auth.CSRF(token), Courses: courses})
}
func (a *application) course(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	course, err := a.Store.Course(r.Context(), user, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	list, err := a.Store.Resources(r.Context(), user, course.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	_, err = a.Store.UploadCourse(r.Context(), user, course.ID)
	canUpload := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	sessions, err := a.Store.ListAttendanceSessions(r.Context(), user, course.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	var rooms []store.Room
	if canUpload {
		rooms, err = a.Store.AttendanceRooms(r.Context(), user, course.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, "Temporarily unavailable", 503)
			return
		}
	}
	a.render(w, 200, "course.html", view{Title: course.Code, User: &user, CSRF: auth.CSRF(token), Course: &course, Resources: list, CanUpload: canUpload, AttendanceSessions: sessions, Rooms: rooms, CanManageAttendance: canUpload})
}
func (a *application) coursesAPI(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	courses, err := a.Store.Courses(r.Context(), user)
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Courses []store.Course `json:"courses"`
	}{courses})
}
func (a *application) admin(w http.ResponseWriter, r *http.Request) (store.User, string, bool) {
	user, token, ok := a.current(w, r)
	if !ok {
		return user, token, false
	}
	if user.Role != "admin" {
		http.Error(w, "Administrator access required", 403)
		return user, token, false
	}
	return user, token, true
}
func (a *application) accountPage(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	a.render(w, 200, "accounts.html", view{Title: "Create an account", User: &user, CSRF: auth.CSRF(token)})
}
func (a *application) createAccount(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	role := r.PostForm.Get("role")
	password := r.PostForm.Get("password")
	address, err := mail.ParseAddress(email)
	data := view{Title: "Create an account", User: &user, CSRF: auth.CSRF(token)}
	if name == "" || len(name) > 100 || len(email) > 254 || err != nil || address.Address != email || (role != "student" && role != "lecturer" && role != "admin") || len(password) < 12 || len(password) > 128 {
		data.Error = "Enter a name, a valid email, a role, and a password of 12–128 bytes."
		a.render(w, 400, "accounts.html", data)
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		http.Error(w, "Please try again shortly", 429)
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	_, err = a.Store.CreateUser(r.Context(), user, name, email, role, hash)
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		data.Error = "An account with that email already exists."
		a.render(w, 409, "accounts.html", data)
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Administrator access required", 403)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	data.Message = "Account created. Share the initial password with the account holder through your approved channel."
	a.render(w, 201, "accounts.html", data)
}
