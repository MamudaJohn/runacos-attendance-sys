package store

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5"
)

func evidenceSpectrum() []float64 { return []float64{.05, .1, .1, .15, .2, .2, .15, .05} }
func TestEvidenceValidationAndDistance(t *testing.T) {
	valid := EvidenceInput{Consent: true, CaptureMethod: "acoustic", SpectralFeatures: evidenceSpectrum()}
	if !validEvidence(valid) {
		t.Fatal("valid evidence rejected")
	}
	for _, test := range []EvidenceInput{
		{Consent: false, CaptureMethod: "manual"},
		{Consent: true, CaptureMethod: "gps"},
		{Consent: true, CaptureMethod: "acoustic", SpectralFeatures: []float64{1}},
		{Consent: true, CaptureMethod: "manual", GPS: &GPSObservation{Latitude: 91}},
		{Consent: true, CaptureMethod: "manual", GPS: &GPSObservation{Longitude: math.NaN()}},
		{Consent: true, CaptureMethod: "manual", GPS: &GPSObservation{AccuracyMeters: -1}},
		{Consent: true, CaptureMethod: "ble"},
		{Consent: true, CaptureMethod: "ble", BLE: &BLEObservation{ServiceUUID: "other", ChallengeReceived: true}},
	} {
		if validEvidence(test) {
			t.Fatalf("accepted %#v", test)
		}
	}
	if d := spectralDistance(evidenceSpectrum(), evidenceSpectrum()); d > 1e-10 {
		t.Fatalf("same spectrum distance %v", d)
	}
	if d := GPSDistanceMeters(0, 0, 0, 1); math.Abs(d-111195) > 5 {
		t.Fatalf("one degree distance %v", d)
	}
	if d := GPSDistanceMeters(90, -180, -90, 180); !finite(d) || d < 20000000 {
		t.Fatalf("antipodal distance %v", d)
	}
}

func TestEvidenceAuthorityOwnershipAndShadow(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	input := EvidenceInput{Consent: true, CaptureMethod: "acoustic", SpectralFeatures: evidenceSpectrum(), GPS: &GPSObservation{Latitude: 6.5, Longitude: 3.4, AccuracyMeters: 75}}
	if _, err := s.RecordAttendanceEvidence(ctx, f.student, f.session.ID, input); !errors.Is(err, ErrAttendanceState) {
		t.Fatalf("draft capture %v", err)
	}
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	calibration := CalibrationInput{Consent: true, SpectralFeatures: evidenceSpectrum(), GPS: &GPSObservation{Latitude: 6.5, Longitude: 3.4, AccuracyMeters: 15}, RadiusMeters: 30}
	for _, actor := range []User{f.student, f.stranger, {ID: f.student.ID, Role: "admin"}} {
		if err := s.SaveRoomCalibration(ctx, actor, f.session.ID, calibration); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized calibration %v", err)
		}
	}
	if err := s.SaveRoomCalibration(ctx, f.lecturer, f.session.ID, calibration); err != nil {
		t.Fatal(err)
	}
	evidence, err := s.RecordAttendanceEvidence(ctx, f.student, f.session.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Comparison.Mode != "shadow" || !evidence.Comparison.CalibrationAvailable || *evidence.Comparison.SpectralCosineDistance > 1e-10 || *evidence.Comparison.GPSDistanceMeters != 0 || *evidence.Comparison.GPSAccuracyMeters != 75 {
		t.Fatalf("comparison %+v", evidence)
	}
	// Even a perfect spectral/location match creates no attendance record.
	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("shadow marked attendance: %d %v", count, err)
	}
	for _, actor := range []User{f.stranger, f.lecturer, {ID: f.stranger.ID, Role: "student"}} {
		if _, err := s.RecordAttendanceEvidence(ctx, actor, f.session.ID, input); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unauthorized evidence %v", err)
		}
	}
	for _, actor := range []User{f.lecturer, f.admin, f.student} {
		items, err := s.EvidenceSummary(ctx, actor, f.session.ID)
		if err != nil || len(items) != 1 || items[0].ID != evidence.ID {
			t.Fatalf("summary %+v %v", items, err)
		}
	}
	if _, err := s.EvidenceSummary(ctx, f.stranger, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("outsider summary %v", err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkAttendanceEvidenceTx(ctx, tx, f.student.ID, f.session.ID, evidence.ID, "acoustic"); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkAttendanceEvidenceTx(ctx, tx, f.stranger.ID, f.session.ID, evidence.ID, "acoustic"); !errors.Is(err, ErrAttendanceInvalid) {
		t.Fatalf("wrong owner accepted %v", err)
	}
	if err := s.LinkAttendanceEvidenceTx(ctx, tx, f.student.ID, f.session.ID, evidence.ID, "qr"); !errors.Is(err, ErrAttendanceInvalid) {
		t.Fatalf("wrong capture method accepted %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE attendance_evidence SET created_at=clock_timestamp()-interval '3 minutes' WHERE id=$1`, evidence.ID)
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkAttendanceEvidenceTx(ctx, tx, f.student.ID, f.session.ID, evidence.ID, "acoustic"); !errors.Is(err, ErrAttendanceInvalid) {
		t.Fatalf("stale evidence accepted %v", err)
	}
	tx.Rollback(ctx)
	mustExec(t, s, `UPDATE attendance_evidence SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, evidence.ID)
	if count, err := s.PruneAttendanceEvidence(ctx); err != nil || count != 1 {
		t.Fatalf("prune %d %v", count, err)
	}
	var raw string
	if err := s.Pool.QueryRow(ctx, `SELECT observation::text FROM attendance_evidence WHERE id=$1`, evidence.ID).Scan(&raw); err != nil || raw != "{}" {
		t.Fatalf("PII retained %s %v", raw, err)
	}
	mustExec(t, s, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, f.student.ID)
	if _, err := s.EvidenceSummary(ctx, f.student, f.session.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("withdrawn summary %v", err)
	}
}

func TestEvidenceBoundedCaptureAndCalibrationRoom(t *testing.T) {
	f := attendanceFixture(t)
	s, ctx := f.store, context.Background()
	if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	input := EvidenceInput{Consent: true, CaptureMethod: "qr"}
	for i := 0; i < 20; i++ {
		if _, err := s.RecordAttendanceEvidence(ctx, f.student, f.session.ID, input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordAttendanceEvidence(ctx, f.student, f.session.ID, input); !errors.Is(err, ErrAttendanceLimit) {
		t.Fatalf("unbounded capture %v", err)
	}
	another, err := s.SaveRoom(ctx, f.admin, "", "Different room", "room", 20)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateAttendanceSession(ctx, f.admin, f.course.ID, another.ID, "Another room session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRoomCalibration(ctx, f.lecturer, session.ID, CalibrationInput{Consent: true, SpectralFeatures: evidenceSpectrum()}); err != nil {
		t.Fatal(err)
	}
	var room string
	if err := s.Pool.QueryRow(ctx, `SELECT room_id::text FROM room_calibrations WHERE session_id=$1`, session.ID).Scan(&room); err != nil || room != another.ID {
		t.Fatalf("wrong room %s %v", room, err)
	}
}
