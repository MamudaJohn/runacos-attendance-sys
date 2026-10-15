package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/jackc/pgx/v5"
)

func (a *application) registerIdentityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/identity", a.identityStatus)
	mux.HandleFunc("POST /api/v1/identity/register/options", a.identityRegistrationOptions)
	mux.HandleFunc("POST /api/v1/identity/register/finish", a.identityRegistrationFinish)
	mux.HandleFunc("POST /api/v1/identity/credentials/{credential}/revoke", a.identityRevoke)
	mux.HandleFunc("POST /api/v1/admin/users/{id}/identity/reset", a.identityReset)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/identity/options", a.identityAttendanceOptions)
	mux.HandleFunc("POST /api/v1/attendance/sessions/{id}/identity/finish", a.identityAttendanceFinish)
}

type identityInput struct {
	Password      string          `json:"password"`
	Nonce         string          `json:"nonce"`
	TransactionID string          `json:"transaction_id"`
	Credential    json.RawMessage `json:"credential"`
	Reason        string          `json:"reason"`
}

func identityError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrIdentityRejected):
		http.Error(w, "Phone identity confirmation was rejected or expired; start a fresh confirmation", 401)
	case errors.Is(err, store.ErrIdentityLimit):
		http.Error(w, "Too many identity requests or enrolled authenticators; try later or contact an administrator", 429)
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
	default:
		attendanceError(w, r, err)
	}
}

// Identity requests need the configured exact origin as well as the logged-in
// session's CSRF token. Host/Forwarded headers cannot widen the WebAuthn origin.
func (a *application) identityJSON(w http.ResponseWriter, r *http.Request, token string, input *identityInput, allowed ...string) bool {
	if a.Identity == nil {
		http.Error(w, "Phone identity confirmation needs a configured HTTPS origin and RP ID", 503)
		return false
	}
	if r.Header.Get("Origin") != a.Identity.Origin() || !a.sameOrigin(r) || !auth.ValidCSRF(token, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "Invalid form token or request origin", 403)
		return false
	}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		http.Error(w, "Expected JSON request", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20*1024)
	d := json.NewDecoder(r.Body)
	var raw map[string]json.RawMessage
	if err := d.Decode(&raw); err != nil {
		attendanceDecodeError(w, err)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF || raw == nil {
		http.Error(w, "Invalid JSON identity request", 400)
		return false
	}
	fields := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		fields[key] = true
	}
	for key := range raw {
		if !fields[key] {
			http.Error(w, "Unexpected identity field", 400)
			return false
		}
	}
	encoded, _ := json.Marshal(raw)
	if err := json.Unmarshal(encoded, input); err != nil {
		http.Error(w, "Invalid JSON identity request", 400)
		return false
	}
	return true
}

func (a *application) identityPassword(w http.ResponseWriter, r *http.Request, user store.User, password string) bool {
	if len(password) > auth.MaxPasswordBytes || !a.accountLimiter.Allow("identity:"+user.ID) {
		http.Error(w, "Too many identity password attempts; try later", 429)
		return false
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		http.Error(w, "Please try again shortly", 429)
		return false
	}
	current, hash, err := a.Store.UserByEmail(r.Context(), user.Email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Temporarily unavailable", 503)
		return false
	}
	if err != nil || current.ID != user.ID || !auth.VerifyPassword(hash, password) {
		http.Error(w, "Current password is incorrect", 401)
		return false
	}
	return true
}

func (a *application) identityStatus(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	if a.Identity == nil {
		writeAttendanceJSON(w, 200, map[string]any{"available": false, "credentials": []any{}, "reason": "Configure a trusted HTTPS origin to enable phone identity confirmation."})
		return
	}
	if user.Role != "student" {
		http.Error(w, "Student account required", 403)
		return
	}
	credentials, err := a.Identity.Credentials(r.Context(), user)
	if err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, map[string]any{"available": true, "credentials": credentials, "origin": a.Identity.Origin(), "user_verification": "required", "notice": "Your phone may use fingerprint, face or PIN. No biometric data is sent to CABV. Enrollment does not prove institutional identity."})
}

func (a *application) identityRegistrationOptions(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "password") {
		return
	}
	if user.Role != "student" {
		http.Error(w, "Student account required", 403)
		return
	}
	if !a.identityPassword(w, r, user, input.Password) {
		return
	}
	options, err := a.Identity.BeginRegistration(r.Context(), user, auth.HashToken(token))
	if err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, options)
}

func (a *application) identityRegistrationFinish(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "transaction_id", "credential") {
		return
	}
	credential, err := a.Identity.FinishRegistration(r.Context(), user, auth.HashToken(token), input.TransactionID, input.Credential)
	if err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 201, credential)
}

func (a *application) identityAttendanceOptions(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "nonce") {
		return
	}
	options, err := a.Identity.BeginAttendance(r.Context(), user, r.PathValue("id"), input.Nonce, auth.HashToken(token))
	if err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, options)
}

func (a *application) identityAttendanceFinish(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "transaction_id", "credential") {
		return
	}
	proof, err := a.Identity.FinishAttendance(r.Context(), user, r.PathValue("id"), auth.HashToken(token), input.TransactionID, input.Credential)
	if err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, proof)
}

func (a *application) identityRevoke(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "password") {
		return
	}
	if !a.identityPassword(w, r, user, input.Password) {
		return
	}
	if err := a.Store.RevokeIdentityCredential(r.Context(), user, a.Identity.RPID(), r.PathValue("credential")); err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, map[string]string{"status": "revoked"})
}

func (a *application) identityReset(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input identityInput
	if !a.identityJSON(w, r, token, &input, "password", "reason") {
		return
	}
	if user.Role != "admin" {
		http.Error(w, "Administrator account required", 403)
		return
	}
	if !a.identityPassword(w, r, user, input.Password) {
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if err := a.Store.ResetStudentIdentity(r.Context(), user, r.PathValue("id"), a.Identity.RPID(), reason); err != nil {
		identityError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, map[string]string{"status": "reset"})
}
