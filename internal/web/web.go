package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata"

	"cabv/internal/auth"
	"cabv/internal/resources"
	"cabv/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Options struct {
	Store         *store.Store
	SecureCookies bool
	Files         *resources.Storage
}

type application struct {
	Options
	pages          *template.Template
	limiter        *auth.LoginLimiter
	accountLimiter *auth.LoginLimiter
	hashSlots      chan struct{}
	dummyHash      string
	resources      *resources.Storage
	uploadSlots    chan struct{}
}

func Handler(ping func(context.Context) error, options ...Options) http.Handler {
	var opt Options
	if len(options) > 0 {
		opt = options[0]
	}
	dummy, err := auth.HashPassword("invalid-account-placeholder")
	if err != nil {
		panic(err)
	}
	zone, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		panic(err)
	}
	pages := template.Must(template.New("").Funcs(template.FuncMap{"localTime": func(t time.Time) string {
		if t.IsZero() {
			return "Not started"
		}
		return t.In(zone).Format("2 Jan 2006, 15:04:05 WAT")
	}, "attendanceStatus": attendanceStatusLabel, "attendanceReason": attendanceReasonLabel}).ParseFS(assets, "templates/*.html"))
	a := &application{Options: opt, pages: pages, limiter: auth.NewLoginLimiterWithLimit(1000), accountLimiter: auth.NewLoginLimiter(), hashSlots: make(chan struct{}, 2), dummyHash: dummy}
	a.resources = opt.Files
	a.uploadSlots = make(chan struct{}, 2)
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		a.render(w, 200, "home.html", view{Title: "CABV Attendance"})
	})
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /auth/login", a.login)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /dashboard", a.dashboard)
	mux.HandleFunc("GET /courses", a.dashboard)
	mux.HandleFunc("GET /courses/{id}", a.course)
	mux.HandleFunc("GET /api/v1/courses", a.coursesAPI)
	mux.HandleFunc("GET /admin/users", a.accountPage)
	mux.HandleFunc("POST /admin/users", a.createAccount)
	mux.HandleFunc("GET /admin/catalog", a.catalog)
	mux.HandleFunc("GET /admin/courses/{id}", a.manageCourse)
	mux.HandleFunc("POST /admin/courses", a.saveCourse)
	mux.HandleFunc("POST /admin/rooms", a.saveRoom)
	mux.HandleFunc("POST /admin/courses/{id}/lecturers", a.assignLecturer)
	mux.HandleFunc("POST /admin/courses/{id}/enrollments", a.setEnrollment)
	mux.HandleFunc("POST /courses/{id}/resources", a.uploadResource)
	mux.HandleFunc("GET /resources/{id}/download", a.downloadResource)
	mux.HandleFunc("POST /attendance/sessions", a.createAttendance)
	mux.HandleFunc("GET /attendance/sessions/{id}", a.attendancePage)
	mux.HandleFunc("POST /attendance/sessions/{id}/open", a.openAttendance)
	mux.HandleFunc("POST /attendance/sessions/{id}/close", a.closeAttendance)
	mux.HandleFunc("POST /attendance/sessions/{id}/challenge", a.issueChallenge)
	mux.HandleFunc("POST /attendance/sessions/{id}/nonce", a.issueNonce)
	mux.HandleFunc("POST /attendance/sessions/{id}/verify", a.verifyAttendance)
	mux.HandleFunc("POST /attendance/sessions/{id}/reviews", a.reviewAttendance)
	mux.HandleFunc("POST /api/v1/attendance/sessions", a.createAttendance)
	mux.HandleFunc("GET /api/v1/attendance/sessions/{id}", a.attendanceMetadata)
	mux.HandleFunc("GET /api/v1/attendance/sessions/{id}/results", a.attendanceResults)
	mux.HandleFunc("GET /api/v1/attendance/sessions/{id}/mine", a.myAttendance)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/open", a.openAttendance)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/close", a.closeAttendance)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/challenge", a.issueChallenge)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/nonce", a.issueNonce)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/verify", a.verifyAttendance)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/reviews", a.reviewAttendance)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { status(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if ping(ctx) != nil {
			status(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		status(w, http.StatusOK, "ready")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		timeout := 5 * time.Second
		if (r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/courses/") && strings.HasSuffix(r.URL.Path, "/resources")) || (strings.HasPrefix(r.URL.Path, "/resources/") && strings.HasSuffix(r.URL.Path, "/download")) {
			timeout = 2 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func status(w http.ResponseWriter, code int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"status":"` + value + `"}` + "\n"))
}
