package web

import (
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"cabv/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (a *application) catalog(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	data := view{Title: "Courses and rooms", User: &user, CSRF: auth.CSRF(token)}
	courses, err := a.Store.Courses(r.Context(), user)
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	rooms, err := a.Store.Rooms(r.Context(), user)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Administrator access required", 403)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	data.Courses = courses
	data.Rooms = rooms
	if r.URL.Query().Get("saved") == "1" {
		data.Message = "Changes saved."
	}
	a.render(w, 200, "catalog.html", data)
}

func (a *application) manageCourse(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
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
	members, err := a.Store.CourseMembers(r.Context(), user, course.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	data := view{Title: "Manage " + course.Code, User: &user, CSRF: auth.CSRF(token), Course: &course, Members: members}
	if r.URL.Query().Get("saved") == "1" {
		data.Message = "Changes saved."
	}
	a.render(w, 200, "manage-course.html", data)
}

func catalogError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Course, room, or eligible account not found", 404)
		return
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23505":
			http.Error(w, "A course code or room name with that value already exists", 409)
			return
		case "23514", "23502", "22001":
			http.Error(w, "Invalid values; review the form", 400)
			return
		}
	}
	http.Error(w, "Temporarily unavailable", 503)
}

func (a *application) saveCourse(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(r.PostForm.Get("code")))
	title := strings.TrimSpace(r.PostForm.Get("title"))
	if code == "" || len(code) > 32 || title == "" || len(title) > 200 {
		http.Error(w, "Enter a course code (up to 32 bytes) and title (up to 200 bytes)", 400)
		return
	}
	course, err := a.Store.SaveCourse(r.Context(), user, r.PostForm.Get("id"), code, title)
	if err != nil {
		catalogError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/courses/"+course.ID+"?saved=1", 303)
}

func (a *application) saveRoom(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	kind := r.PostForm.Get("kind")
	capacity, err := strconv.Atoi(r.PostForm.Get("capacity"))
	if name == "" || len(name) > 100 || (kind != "room" && kind != "theatre") || err != nil || capacity < 1 || capacity > 10000 {
		http.Error(w, "Enter a room name, room/theatre type, and capacity from 1 to 10000", 400)
		return
	}
	if _, err := a.Store.SaveRoom(r.Context(), user, r.PostForm.Get("id"), name, kind, capacity); err != nil {
		catalogError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/catalog?saved=1", 303)
}

func memberEmail(r *http.Request) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(r.PostForm.Get("email")))
	address, err := mail.ParseAddress(email)
	return email, err == nil && address.Address == email && len(email) <= 254
}

func (a *application) assignLecturer(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	email, valid := memberEmail(r)
	assignment := r.PostForm.Get("assignment")
	if !valid || (assignment != "assigned" && assignment != "removed") {
		http.Error(w, "Enter a lecturer email and valid assignment action", 400)
		return
	}
	id := r.PathValue("id")
	if err := a.Store.AssignLecturer(r.Context(), user, id, email, assignment == "assigned"); err != nil {
		catalogError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/courses/"+id+"?saved=1", 303)
}

func (a *application) setEnrollment(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.admin(w, r)
	if !ok {
		return
	}
	if !a.form(w, r, token) {
		return
	}
	email, valid := memberEmail(r)
	status := r.PostForm.Get("status")
	if !valid || (status != "active" && status != "withdrawn") {
		http.Error(w, "Enter a student email and active/withdrawn enrollment status", 400)
		return
	}
	id := r.PathValue("id")
	if err := a.Store.SetEnrollment(r.Context(), user, id, email, status); err != nil {
		catalogError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/courses/"+id+"?saved=1", 303)
}
