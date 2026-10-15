package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"cabv/internal/auth"
	"cabv/internal/store"
	qrcode "github.com/skip2/go-qrcode"
)

// qrDataPNG is only called on the manager-only challenge issuance response.
// The challenge is never exposed by student metadata or a cacheable resource URL.
func qrDataPNG(payload string) (string, error) {
	if len(payload) > 512 {
		return "", store.ErrAttendanceInvalid
	}
	encoded, err := qrcode.Encode(payload, qrcode.Medium, 384)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded), nil
}

func (a *application) sensorInput(w http.ResponseWriter, r *http.Request, token string, input any) bool {
	if !a.sameOrigin(r) || !auth.ValidCSRF(token, r.Header.Get("X-CSRF-Token")) {
		http.Error(w, "Invalid form token or request origin", 403)
		return false
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		http.Error(w, "Expected JSON request", 415)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(input); err != nil {
		sensorDecodeError(w, err)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		sensorDecodeError(w, err)
		return false
	}
	return true
}
func sensorDecodeError(w http.ResponseWriter, err error) {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		http.Error(w, "Sensor request is too large", 413)
		return
	}
	http.Error(w, "Invalid sensor request", 400)
}

func (a *application) recordEvidence(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input store.EvidenceInput
	if !a.sensorInput(w, r, token, &input) {
		return
	}
	evidence, err := a.Store.RecordAttendanceEvidence(r.Context(), user, r.PathValue("id"), input)
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 201, evidence)
}
func (a *application) calibrateRoom(w http.ResponseWriter, r *http.Request) {
	user, token, ok := a.current(w, r)
	if !ok {
		return
	}
	var input store.CalibrationInput
	if !a.sensorInput(w, r, token, &input) {
		return
	}
	if err := a.Store.SaveRoomCalibration(r.Context(), user, r.PathValue("id"), input); err != nil {
		attendanceError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, map[string]any{"mode": "shadow", "saved": true, "message": "Experimental room reference saved; it does not verify classroom presence."})
}
func (a *application) attendanceEvidence(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.current(w, r)
	if !ok {
		return
	}
	records, err := a.Store.EvidenceSummary(r.Context(), user, r.PathValue("id"))
	if err != nil {
		attendanceError(w, r, err)
		return
	}
	writeAttendanceJSON(w, 200, map[string]any{"mode": "shadow", "evidence": records})
}
