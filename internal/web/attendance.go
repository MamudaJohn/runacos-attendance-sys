package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/jackc/pgx/v5"
)

type attendanceInput struct {
	CourseID        string `json:"course_id"`
	RoomID          string `json:"room_id"`
	Title           string `json:"title"`
	DurationMinutes int    `json:"duration_minutes"`
	Token           string `json:"token"`
	Nonce           string `json:"nonce"`
	Email           string `json:"email"`
	Decision        string `json:"decision"`
	Reason          string `json:"reason"`
	CaptureMethod   string `json:"capture_method"`
	EvidenceID      string `json:"evidence_id"`
	IdentityProofID string `json:"identity_proof_id"`
}

func attendanceAPI(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/v1/") }
func writeAttendanceJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
func (a *application) attendanceForm(w http.ResponseWriter, r *http.Request, token string) bool {
	if !attendanceAPI(r) {
		return a.form(w, r, token)
	}
	if !a.sameOrigin(r) || !auth.ValidCSRF(token, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "Invalid form token or request origin", 403)
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		http.Error(w, "Expected JSON request", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		attendanceDecodeError(w, err)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		attendanceDecodeError(w, err)
		return false
	}
	if fields == nil {
		http.Error(w, "Expected a JSON object", 400)
		return false
	}
	allowed := map[string]bool{}
	switch {
	case strings.HasSuffix(r.URL.Path, "/sessions"):
		for _, key := range []string{"course_id", "room_id", "title", "duration_minutes"} {
			allowed[key] = true
		}
	case strings.HasSuffix(r.URL.Path, "/verify"):
		allowed["token"] = true
		allowed["nonce"] = true
		allowed["capture_method"] = true
		allowed["evidence_id"] = true
		allowed["identity_proof_id"] = true
	case strings.HasSuffix(r.URL.Path, "/reviews"):
		allowed["email"] = true
		allowed["decision"] = true
		allowed["reason"] = true
	}
	for key := range fields {
		if !allowed[key] {
			http.Error(w, "Unexpected attendance field", 400)
			return false
		}
	}
	encoded, _ := json.Marshal(fields)
	var input attendanceInput
	if err := json.Unmarshal(encoded, &input); err != nil {
		attendanceDecodeError(w, err)
		return false
	}
	r.PostForm = url.Values{"course_id": {input.CourseID}, "room_id": {input.RoomID}, "title": {input.Title}, "duration_minutes": {strconv.Itoa(input.DurationMinutes)}, "token": {input.Token}, "nonce": {input.Nonce}, "email": {input.Email}, "decision": {input.Decision}, "reason": {input.Reason}}
	r.PostForm.Set("capture_method", input.CaptureMethod)
	r.PostForm.Set("evidence_id", input.EvidenceID)
	r.PostForm.Set("identity_proof_id", input.IdentityProofID)
	return true
}
func attendanceDecodeError(w http.ResponseWriter, err error) {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		http.Error(w, "Attendance request is too large", 413)
		return
	}
	http.Error(w, "Invalid JSON attendance request", 400)
}
func attendanceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
	case errors.Is(err, store.ErrAttendanceState):
		http.Error(w, "The session is not available for that action", 409)
	case errors.Is(err, store.ErrAttendanceInvalid):
		http.Error(w, "Invalid attendance values", 400)
	case errors.Is(err, store.ErrAttendanceLimit):
		http.Error(w, "Session attempt limit reached; contact your lecturer", 429)
	default:
		http.Error(w, "Temporarily unavailable", 503)
	}
}
func attendanceSessionJSON(s store.AttendanceSession) any {
	return struct {
		ID               string `json:"id"`
		CourseID         string `json:"course_id"`
		RoomID           string `json:"room_id"`
		Title            string `json:"title"`
		Status           string `json:"status"`
		DurationMinutes  int    `json:"duration_minutes"`
		StartsAt         any    `json:"starts_at"`
		EndsAt           any    `json:"ends_at"`
		PolicyVersion    string `json:"policy_version"`
		IdentityRequired bool   `json:"identity_required"`
	}{s.ID, s.CourseID, s.RoomID, s.Title, s.Status, s.DurationMinutes, nullableAttendanceTime(s.StartsAt), nullableAttendanceTime(s.EndsAt), s.PolicyVersion, s.IdentityRequired}
}
func nullableAttendanceTime(t interface{ IsZero() bool }) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func attendanceResultJSON(result store.AttendanceResult) any {
	return struct {
		StudentID    string `json:"student_id"`
		Email        string `json:"email"`
		Name         string `json:"name"`
		Status       string `json:"status"`
		Source       string `json:"source"`
		ReasonCode   string `json:"reason_code"`
		ReviewReason string `json:"review_reason"`
		ReviewedBy   string `json:"reviewed_by"`
		ReviewerName string `json:"reviewer_name"`
		CanReview    bool   `json:"can_review"`
	}{result.StudentID, result.Email, result.Name, result.Status, result.Source, result.ReasonCode, result.ReviewReason, result.ReviewedBy, result.ReviewerName, result.CanReview}
}
func (a *application) attendanceView(w http.ResponseWriter, r *http.Request, user store.User, token, id string) (view, bool) {
	session, err := a.Store.AttendanceSession(r.Context(), user, id)
	if err != nil {
		attendanceError(w, r, err)
		return view{}, false
	}
	_, err = a.Store.UploadCourse(r.Context(), user, session.CourseID)
	manager := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		attendanceError(w, r, err)
		return view{}, false
	}
	data := view{Title: session.Title, User: &user, CSRF: auth.CSRF(token), AttendanceSession: &session, CanManageAttendance: manager, IdentityEnabled: a.Identity != nil || session.IdentityRequired}
	if manager {
		results, err := a.Store.AttendanceResults(r.Context(), user, id)
		if err != nil {
			attendanceError(w, r, err)
			return view{}, false
		}
		data.AttendanceResults = results
	} else if user.Role == "student" {
		mine, err := a.Store.MyAttendance(r.Context(), user, id)
		if err != nil {
			attendanceError(w, r, err)
			return view{}, false
		}
		data.MyAttendance = &mine
		data.CanCheckIn = session.Status == "open" && mine.Status != "pending" && mine.Status != "present" && mine.Source != "manual"
	}
	return data, true
}
func (a *application) attendancePage(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	data, ok := a.attendanceView(w, r, user, token, r.PathValue("id"))
	if !ok {
		return
	}
	a.render(w, 200, "attendance.html", data)
}

func attendanceStatusLabel(status string) string {
	switch status {
	case "not_submitted":
		return "Not submitted"
	case "pending":
		return "Pending lecturer review"
	case "present":
		return "Present (manual review)"
	case "rejected":
		return "Rejected"
	}
	return status
}
func attendanceReasonLabel(reason string) string {
	switch reason {
	case "pending_review":
		return "The challenge was accepted and awaits lecturer review."
	case "nonce_used":
		return "This check-in form has already been used."
	case "nonce_invalid":
		return "This check-in form is invalid. Start a fresh check-in."
	case "nonce_expired":
		return "This check-in form expired. Start a fresh check-in."
	case "challenge_invalid":
		return "The challenge is incorrect. Request the current challenge from your lecturer."
	case "challenge_revoked":
		return "Your lecturer replaced this challenge. Use the current challenge."
	case "challenge_expired":
		return "The challenge expired. Request a fresh challenge."
	case "session_closed":
		return "Attendance has closed. Contact your lecturer for review."
	case "session_expired":
		return "The attendance window has ended. Contact your lecturer for review."
	case "session_not_open":
		return "Attendance has not opened."
	case "manual_review":
		return "The lecturer has recorded a manual decision."
	case "duplicate":
		return "Your existing attendance record is retained."
	case "identity_required":
		return "Confirm with your registered phone authenticator before submitting attendance."
	case "identity_rejected":
		return "Phone confirmation was invalid, expired or used. Start a fresh check-in."
	case "evidence_invalid":
		return "Captured evidence was invalid, expired or used. Capture it again."
	}
	return "The request could not be accepted. Contact your lecturer."
}
func (a *application) attendanceMetadata(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	session, err := a.Store.AttendanceSession(r.Context(), user, r.PathValue("id"))
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, attendanceSessionJSON(session))
}
func (a *application) createAttendance(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	minutes, err := strconv.Atoi(r.PostForm.Get("duration_minutes"))
	title := strings.TrimSpace(r.PostForm.Get("title"))
	if err != nil || minutes < 1 || minutes > 180 || title == "" || len(title) > 200 {
		http.Error(w, "Enter a title and duration from 1 to 180 minutes", 400)
		return
	}
	session, err := a.Store.CreateAttendanceSession(r.Context(), user, r.PostForm.Get("course_id"), r.PostForm.Get("room_id"), title, minutes, a.Identity != nil)
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		writeAttendanceJSON(w, 201, attendanceSessionJSON(session))
		return
	}
	http.Redirect(w, r, "/attendance/sessions/"+session.ID, 303)
}
func (a *application) attendanceMutation(w http.ResponseWriter, r *http.Request, open bool) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	id := r.PathValue("id")
	var err error
	if open {
		err = a.Store.OpenAttendanceSession(r.Context(), user, id, a.Identity != nil)
	} else {
		err = a.Store.CloseAttendanceSession(r.Context(), user, id)
	}
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		session, err := a.Store.AttendanceSession(r.Context(), user, id)
		if err != nil {
			attendanceError(w, r, err)
			return
		}
		writeAttendanceJSON(w, 200, attendanceSessionJSON(session))
		return
	}
	http.Redirect(w, r, "/attendance/sessions/"+id, 303)
}
func (a *application) openAttendance(w http.ResponseWriter, r *http.Request) {
	a.attendanceMutation(w, r, true)
}
func (a *application) closeAttendance(w http.ResponseWriter, r *http.Request) {
	a.attendanceMutation(w, r, false)
}
func (a *application) issueChallenge(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	id := r.PathValue("id")
	challenge, err := a.Store.IssueAttendanceChallenge(r.Context(), user, id, a.Identity != nil)
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	payload, _ := json.Marshal(map[string]any{"v": 1, "session_id": id, "token": challenge.Token, "expires_at": challenge.ExpiresAt})
	qr, err := qrDataPNG(string(payload))
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		writeAttendanceJSON(w, 200, map[string]any{"token": challenge.Token, "generation": challenge.Generation, "expires_at": challenge.ExpiresAt, "qr_image": qr, "qr_payload": string(payload)})
		return
	}
	data, ok := a.attendanceView(w, r, user, token, id)
	if !ok {
		return
	}
	data.Challenge = &challenge
	data.ChallengeQR = qr
	a.render(w, 200, "attendance.html", data)
}
func (a *application) issueNonce(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	id := r.PathValue("id")
	nonce, err := a.Store.Nonce(r.Context(), user, id)
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		writeAttendanceJSON(w, 200, map[string]any{"nonce": nonce.Nonce, "expires_at": nonce.ExpiresAt})
		return
	}
	data, ok := a.attendanceView(w, r, user, token, id)
	if !ok {
		return
	}
	data.AttendanceNonce = &nonce
	a.render(w, 200, "attendance.html", data)
}
func validAttendanceToken(s string) bool {
	if len(s) != 43 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func (a *application) verifyAttendance(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	id := r.PathValue("id")
	challenge, nonce := strings.TrimSpace(r.PostForm.Get("token")), strings.TrimSpace(r.PostForm.Get("nonce"))
	if !validAttendanceToken(challenge) || !validAttendanceToken(nonce) {
		http.Error(w, "Enter a valid challenge and fresh check-in nonce", 400)
		return
	}
	rpID := ""
	if a.Identity != nil {
		rpID = a.Identity.RPID()
	}
	decision, err := a.Store.VerifyAttendanceWithEvidence(r.Context(), user, id, challenge, nonce, store.AttendanceSubmission{
		CaptureMethod: r.PostForm.Get("capture_method"), EvidenceID: r.PostForm.Get("evidence_id"), IdentityProofID: r.PostForm.Get("identity_proof_id"), RequireIdentity: a.Identity != nil,
		ExpectedRPID: rpID,
	})
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		writeAttendanceJSON(w, 200, map[string]any{"status": decision.Status, "reason_code": decision.ReasonCode, "duplicate": decision.Duplicate})
		return
	}
	data, ok := a.attendanceView(w, r, user, token, id)
	if !ok {
		return
	}
	data.AttendanceDecision = &decision
	a.render(w, 200, "attendance.html", data)
}
func (a *application) attendanceResults(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	rows, err := a.Store.AttendanceResults(r.Context(), user, r.PathValue("id"))
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	results := make([]any, 0, len(rows))
	for _, row := range rows {
		results = append(results, attendanceResultJSON(row))
	}
	writeAttendanceJSON(w, 200, map[string]any{"results": results})
}
func (a *application) myAttendance(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	result, err := a.Store.MyAttendance(r.Context(), user, r.PathValue("id"))
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, attendanceResultJSON(result))
}
func (a *application) reviewAttendance(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	if !a.attendanceForm(w, r, token) {
		return
	}
	id := r.PathValue("id")
	email, valid := memberEmail(r)
	decision := r.PostForm.Get("decision")
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if !valid || (decision != "present" && decision != "rejected") || reason == "" || len(reason) > 500 {
		http.Error(w, "Select present/rejected and provide a review reason (up to 500 bytes)", 400)
		return
	}
	if err := a.Store.ReviewAttendance(r.Context(), user, id, email, decision, reason); err != nil {
		attendanceError(w, r, err)
		return
	}
	if attendanceAPI(r) {
		writeAttendanceJSON(w, 200, map[string]string{"status": decision, "source": "manual"})
		return
	}
	http.Redirect(w, r, "/attendance/sessions/"+id, 303)
}
