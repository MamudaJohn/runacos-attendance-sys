package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"cabv/internal/auth"
	"cabv/internal/resources"
	"cabv/internal/store"
	"github.com/jackc/pgx/v5"
)

func (a *application) uploadResource(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	course, err := a.Store.UploadCourse(r.Context(), user, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	if !a.sameOrigin(r) {
		http.Error(w, "Invalid request origin", 403)
		return
	}
	if a.resources == nil {
		http.Error(w, "Resource storage unavailable", 503)
		return
	}
	select {
	case a.uploadSlots <- struct{}{}:
		defer func() { <-a.uploadSlots }()
	default:
		w.Header().Set("Retry-After", "10")
		http.Error(w, "Uploads are busy; try again shortly", 429)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, resources.MaxFileBytes+64*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Expected multipart file upload", 415)
		return
	}
	csrfSeen, titleSeen, fileSeen := false, false, false
	var title string
	var saved resources.File
	committed := false
	defer func() {
		if fileSeen && !committed {
			_ = a.resources.Remove(saved.Key)
		}
	}()
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			uploadError(w, err)
			return
		}
		name := part.FormName()
		switch name {
		case "csrf":
			if csrfSeen || fileSeen || part.FileName() != "" {
				http.Error(w, "Invalid form token", 403)
				return
			}
			value, err := io.ReadAll(io.LimitReader(part, 65))
			if err != nil {
				uploadError(w, err)
				return
			}
			if len(value) > 64 || !auth.ValidCSRF(token, string(value)) {
				http.Error(w, "Invalid form token; reload the page", 403)
				return
			}
			csrfSeen = true
		case "title":
			if titleSeen || fileSeen || part.FileName() != "" {
				http.Error(w, "Invalid resource title", 400)
				return
			}
			value, err := io.ReadAll(io.LimitReader(part, 201))
			if err != nil {
				uploadError(w, err)
				return
			}
			title = strings.TrimSpace(string(value))
			if len(value) > 200 || title == "" || !utf8.ValidString(title) || strings.IndexFunc(title, unicode.IsControl) >= 0 {
				http.Error(w, "Enter a title of 1–200 bytes", 400)
				return
			}
			titleSeen = true
		case "file":
			if !csrfSeen {
				http.Error(w, "Form token must precede the file", 403)
				return
			}
			if !titleSeen || fileSeen || part.FileName() == "" {
				http.Error(w, "Upload one file after its title", 400)
				return
			}
			saved, err = a.resources.Save(part, part.FileName())
			if err != nil {
				uploadError(w, err)
				return
			}
			fileSeen = true
		default:
			http.Error(w, "Unexpected upload field", 400)
			return
		}
	}
	if !csrfSeen {
		http.Error(w, "Invalid form token", 403)
		return
	}
	if !titleSeen || !fileSeen {
		http.Error(w, "A title and one file are required", 400)
		return
	}
	_, err = a.Store.CreateResource(r.Context(), user, store.Resource{CourseID: course.ID, Title: title, OriginalName: saved.OriginalName, StorageKey: saved.Key, MIMEType: saved.MIME, SizeBytes: saved.Size})
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Unable to save resource; try again later", 503)
		return
	}
	committed = true
	http.Redirect(w, r, "/courses/"+course.ID, 303)
}

func uploadError(w http.ResponseWriter, err error) {
	var max *http.MaxBytesError
	switch {
	case errors.Is(err, resources.ErrTooLarge), errors.As(err, &max):
		http.Error(w, "File exceeds the 5 MiB limit", 413)
	case errors.Is(err, resources.ErrUnsupportedType):
		http.Error(w, "Upload a PDF or UTF-8 plain text .txt file matching its content", 415)
	case errors.Is(err, resources.ErrQuota):
		http.Error(w, "Resource storage is full; contact an administrator", 507)
	case errors.Is(err, io.ErrUnexpectedEOF):
		http.Error(w, "Incomplete upload", 400)
	default:
		http.Error(w, "Unable to store upload; try again later", 503)
	}
}

func (a *application) downloadResource(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	resource, err := a.Store.Resource(r.Context(), user, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Temporarily unavailable", 503)
		return
	}
	if a.resources == nil {
		http.Error(w, "Resource storage unavailable", 503)
		return
	}
	file, err := a.resources.Open(resource.StorageKey)
	if err != nil {
		http.Error(w, "Resource file unavailable", 503)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != resource.SizeBytes {
		http.Error(w, "Resource file unavailable", 503)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": resource.OriginalName}))
	w.Header().Set("Content-Type", resource.MIMEType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(privateResourceWriter{w}, r, resource.OriginalName, info.ModTime(), file)
}

// ServeContent removes cache headers on range errors; restore the private-file
// policy when it writes any status, including 416 and conditional responses.
type privateResourceWriter struct{ http.ResponseWriter }

func (w privateResourceWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.ResponseWriter.WriteHeader(status)
}
func (w privateResourceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
