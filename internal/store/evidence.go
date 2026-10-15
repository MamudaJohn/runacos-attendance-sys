package store

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// CABV GATT peripheral protocol: UTF-8 attendance QR JSON in a readable characteristic.
// A browser is a central/client; it cannot turn the lecturer's phone into a beacon.
const BLEServiceUUID = "cbaf0001-5c28-4d5f-89f7-19b6c1560001"
const BLEChallengeUUID = "cbaf0002-5c28-4d5f-89f7-19b6c1560001"

type GPSObservation struct {
	Latitude       float64 `json:"latitude"`
	Longitude      float64 `json:"longitude"`
	AccuracyMeters float64 `json:"accuracy_meters"`
}
type BLEObservation struct {
	ServiceUUID       string `json:"service_uuid"`
	ChallengeReceived bool   `json:"challenge_received"`
}
type EvidenceInput struct {
	Consent          bool            `json:"consent"`
	CaptureMethod    string          `json:"capture_method"`
	SpectralFeatures []float64       `json:"spectral_features,omitempty"`
	GPS              *GPSObservation `json:"gps,omitempty"`
	BLE              *BLEObservation `json:"ble,omitempty"`
}
type CalibrationInput struct {
	Consent          bool            `json:"consent"`
	SpectralFeatures []float64       `json:"spectral_features"`
	GPS              *GPSObservation `json:"gps,omitempty"`
	RadiusMeters     float64         `json:"radius_meters,omitempty"`
}
type EvidenceComparison struct {
	Mode                   string   `json:"mode"`
	CalibrationAvailable   bool     `json:"calibration_available"`
	SpectralCosineDistance *float64 `json:"spectral_cosine_distance,omitempty"`
	GPSDistanceMeters      *float64 `json:"gps_distance_meters,omitempty"`
	GPSAccuracyMeters      *float64 `json:"gps_accuracy_meters,omitempty"`
	ReferenceRadiusMeters  *float64 `json:"reference_radius_meters,omitempty"`
	// No inside/outside classifier: GPS, spectra and BLE are spoofable and require room trials.
}
type AttendanceEvidence struct {
	ID            string             `json:"id"`
	StudentID     string             `json:"student_id"`
	CaptureMethod string             `json:"capture_method"`
	Comparison    EvidenceComparison `json:"comparison"`
	CreatedAt     time.Time          `json:"created_at"`
	ExpiresAt     time.Time          `json:"expires_at"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func validGPS(g *GPSObservation) bool {
	return g == nil || (finite(g.Latitude) && finite(g.Longitude) && finite(g.AccuracyMeters) && g.Latitude >= -90 && g.Latitude <= 90 && g.Longitude >= -180 && g.Longitude <= 180 && g.AccuracyMeters >= 0 && g.AccuracyMeters <= 100000)
}
func validSpectrum(v []float64) bool {
	if len(v) != 8 {
		return false
	}
	sum := 0.0
	for _, x := range v {
		if !finite(x) || x < 0 || x > 1 {
			return false
		}
		sum += x
	}
	return math.Abs(sum-1) <= 0.01
}
func validEvidence(input EvidenceInput) bool {
	if !input.Consent || !validGPS(input.GPS) {
		return false
	}
	switch input.CaptureMethod {
	case "manual", "acoustic", "qr", "ble":
	default:
		return false
	}
	if input.SpectralFeatures != nil && !validSpectrum(input.SpectralFeatures) {
		return false
	}
	if input.BLE != nil && (input.BLE.ServiceUUID != BLEServiceUUID || !input.BLE.ChallengeReceived || input.CaptureMethod != "ble") {
		return false
	}
	if input.CaptureMethod == "ble" && input.BLE == nil {
		return false
	}
	return true
}

func spectralDistance(a, b []float64) float64 {
	dot, aa, bb := 0.0, 0.0, 0.0
	for i := range a {
		dot += a[i] * b[i]
		aa += a[i] * a[i]
		bb += b[i] * b[i]
	}
	return math.Max(0, math.Min(1, 1-dot/math.Sqrt(aa*bb)))
}
func GPSDistanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	radians := math.Pi / 180
	dLat, dLon := (lat2-lat1)*radians, (lon2-lon1)*radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*radians)*math.Cos(lat2*radians)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 6371008.8 * 2 * math.Asin(math.Sqrt(math.Min(1, a)))
}

// SaveRoomCalibration rechecks current manager authority and binds the sample to
// the session room. Only the latest profile is retained; no audio is accepted.
func (s *Store) SaveRoomCalibration(ctx context.Context, actor User, sessionID string, input CalibrationInput) error {
	if !input.Consent || !validSpectrum(input.SpectralFeatures) || !validGPS(input.GPS) || (input.GPS != nil && (!finite(input.RadiusMeters) || input.RadiusMeters < 5 || input.RadiusMeters > 10000)) || (input.GPS == nil && input.RadiusMeters != 0) {
		return ErrAttendanceInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceManager(ctx, tx, actor, sessionID)
	if err != nil {
		return err
	}
	var lat, lon, radius any
	if input.GPS != nil {
		lat, lon, radius = input.GPS.Latitude, input.GPS.Longitude, input.RadiusMeters
	}
	encoded, _ := json.Marshal(input.SpectralFeatures)
	_, err = tx.Exec(ctx, `INSERT INTO room_calibrations(room_id,session_id,actor_id,spectral_features,latitude,longitude,radius_meters)
        VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(room_id) DO UPDATE SET session_id=excluded.session_id,actor_id=excluded.actor_id,
        spectral_features=excluded.spectral_features,latitude=excluded.latitude,longitude=excluded.longitude,radius_meters=excluded.radius_meters,updated_at=clock_timestamp()`, session.RoomID, session.ID, actor.ID, encoded, lat, lon, radius)
	if err != nil {
		return err
	}
	if err := auditMutation(ctx, tx, actor.ID, "room.calibration", "room", session.RoomID, map[string]any{"mode": "shadow", "gps_reference": input.GPS != nil}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordAttendanceEvidence(ctx context.Context, actor User, sessionID string, input EvidenceInput) (AttendanceEvidence, error) {
	if !validEvidence(input) {
		return AttendanceEvidence{}, ErrAttendanceInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return AttendanceEvidence{}, err
	}
	defer tx.Rollback(ctx)
	session, err := lockAttendanceStudent(ctx, tx, actor, sessionID)
	if err != nil {
		return AttendanceEvidence{}, err
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT $1='open' AND $2::timestamptz>clock_timestamp()`, session.Status, session.EndsAt).Scan(&open); err != nil {
		return AttendanceEvidence{}, err
	}
	if !open {
		return AttendanceEvidence{}, ErrAttendanceState
	}
	// Scoped opportunistic retention; linked evidence is sanitized after its TTL.
	if _, err := tx.Exec(ctx, `UPDATE attendance_evidence SET observation='{}'::jsonb WHERE session_id=$1 AND expires_at<=clock_timestamp() AND observation<>'{}'::jsonb`, session.ID); err != nil {
		return AttendanceEvidence{}, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM attendance_evidence WHERE session_id=$1 AND student_id=$2 AND expires_at>clock_timestamp()`, session.ID, actor.ID).Scan(&count); err != nil {
		return AttendanceEvidence{}, err
	}
	if count >= 20 {
		return AttendanceEvidence{}, ErrAttendanceLimit
	}
	comparison := EvidenceComparison{Mode: "shadow"}
	var spectrum []byte
	var lat, lon, radius *float64
	err = tx.QueryRow(ctx, `SELECT spectral_features,latitude,longitude,radius_meters FROM room_calibrations WHERE room_id=$1`, session.RoomID).Scan(&spectrum, &lat, &lon, &radius)
	if err != nil && err != pgx.ErrNoRows {
		return AttendanceEvidence{}, err
	}
	if err == nil {
		comparison.CalibrationAvailable = true
		if input.SpectralFeatures != nil {
			var ref []float64
			if err := json.Unmarshal(spectrum, &ref); err != nil {
				return AttendanceEvidence{}, err
			}
			if !validSpectrum(ref) {
				return AttendanceEvidence{}, ErrAttendanceInvalid
			}
			d := spectralDistance(input.SpectralFeatures, ref)
			comparison.SpectralCosineDistance = &d
		}
		if input.GPS != nil && lat != nil && lon != nil && radius != nil {
			d := GPSDistanceMeters(input.GPS.Latitude, input.GPS.Longitude, *lat, *lon)
			comparison.GPSDistanceMeters = &d
			comparison.GPSAccuracyMeters = &input.GPS.AccuracyMeters
			comparison.ReferenceRadiusMeters = radius
		}
	}
	observation, _ := json.Marshal(input)
	summary, _ := json.Marshal(comparison)
	result := AttendanceEvidence{StudentID: actor.ID, CaptureMethod: input.CaptureMethod, Comparison: comparison}
	err = tx.QueryRow(ctx, `INSERT INTO attendance_evidence(session_id,student_id,capture_method,observation,comparison)
        VALUES($1,$2,$3,$4,$5) RETURNING id::text,created_at,expires_at`, session.ID, actor.ID, input.CaptureMethod, observation, summary).Scan(&result.ID, &result.CreatedAt, &result.ExpiresAt)
	if err != nil {
		return AttendanceEvidence{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AttendanceEvidence{}, err
	}
	return result, nil
}

// EvidenceSummary exposes bounded comparisons, never raw coordinates or devices.
// Students can only read their own observations; staff need current course authority.
func (s *Store) EvidenceSummary(ctx context.Context, actor User, sessionID string) ([]AttendanceEvidence, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	id, err := parameterUUID(actor.ID)
	if err != nil {
		return nil, err
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND active`, id).Scan(&role); err != nil {
		return nil, err
	}
	_, err = lockAttendanceSession(ctx, tx, actor, sessionID, role == "student")
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id::text,student_id::text,capture_method,comparison,created_at,expires_at FROM attendance_evidence
        WHERE session_id=$1 AND ($2<>'student' OR student_id=$3) AND expires_at>clock_timestamp() ORDER BY created_at DESC LIMIT 200`, sessionID, role, actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AttendanceEvidence, 0)
	for rows.Next() {
		var item AttendanceEvidence
		var raw []byte
		if err := rows.Scan(&item.ID, &item.StudentID, &item.CaptureMethod, &raw, &item.CreatedAt, &item.ExpiresAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &item.Comparison); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// LinkAttendanceEvidenceTx is called inside attendance verification after locking
// the current student and session. A browser claim cannot choose another student's
// evidence or label a manual attempt acoustic; observations have a two-minute use window.
func (s *Store) LinkAttendanceEvidenceTx(ctx context.Context, tx pgx.Tx, studentID, sessionID, evidenceID, captureMethod string) error {
	if evidenceID == "" {
		if captureMethod == "manual" || captureMethod == "" {
			return nil
		}
		return ErrAttendanceInvalid
	}
	id, err := parameterUUID(evidenceID)
	if err != nil {
		return ErrAttendanceInvalid
	}
	var found string
	err = tx.QueryRow(ctx, `SELECT e.id::text FROM attendance_evidence e WHERE e.id=$1 AND e.session_id=$2 AND e.student_id=$3
        AND e.capture_method=$4 AND e.created_at>clock_timestamp()-interval '2 minutes' AND e.expires_at>clock_timestamp()
        AND NOT EXISTS(SELECT 1 FROM verification_attempts v WHERE v.evidence_id=e.id) FOR UPDATE OF e`, id, sessionID, studentID, captureMethod).Scan(&found)
	if err == pgx.ErrNoRows {
		return ErrAttendanceInvalid
	}
	return err
}

// PruneAttendanceEvidence clears sensitive optional observations after seven days.
// The opaque ID and comparison are retained to preserve the audit/attempt link.
func (s *Store) PruneAttendanceEvidence(ctx context.Context) (int64, error) {
	result, err := s.Pool.Exec(ctx, `UPDATE attendance_evidence SET observation='{}'::jsonb WHERE expires_at<=clock_timestamp() AND observation<>'{}'::jsonb`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected(), nil
}
