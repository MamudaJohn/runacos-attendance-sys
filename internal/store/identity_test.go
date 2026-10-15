package store

import (
	"context"
	"errors"
	"testing"

	"cabv/internal/auth"
)

func identityProofFixture(t *testing.T) (attendanceTestFixture, IdentityCredential, IdentityProof, string, string) {
	t.Helper()
	f, ctx := attendanceFixture(t), context.Background()
	if err := f.store.OpenAttendanceSession(ctx, f.lecturer, f.session.ID); err != nil {
		t.Fatal(err)
	}
	nonce, err := f.store.Nonce(ctx, f.student, f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := f.store.LockIdentityActorTx(ctx, tx, f.student, ""); err != nil {
		t.Fatal(err)
	}
	credential, err := f.store.SaveIdentityCredentialTx(ctx, tx, f.student.ID, "attendance.test", []byte("public-credential-id"), []byte(`{"publicKey":"only public material"}`))
	if err != nil {
		t.Fatal(err)
	}
	nonceHash := auth.HashToken(nonce.Nonce)
	proof, err := f.store.SaveIdentityProofTx(ctx, tx, f.student.ID, f.session.ID, credential.ID, nonceHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f, credential, proof, nonceHash, nonce.Nonce
}

func TestIdentityProofConsumptionBindingAndRollback(t *testing.T) {
	f, _, proof, nonceHash, _ := identityProofFixture(t)
	s, ctx := f.store, context.Background()
	consume := func(student, session, nonce, id string, commit bool) error {
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		err = s.ConsumeIdentityProofTx(ctx, tx, student, session, nonce, id)
		if err == nil && commit {
			err = tx.Commit(ctx)
		}
		return err
	}
	for _, tc := range []struct{ user, session, nonce, id string }{
		{f.stranger.ID, f.session.ID, nonceHash, proof.ID},
		{f.student.ID, f.course.ID, nonceHash, proof.ID},
		{f.student.ID, f.session.ID, auth.HashToken("other nonce"), proof.ID},
		{f.student.ID, f.session.ID, nonceHash, "not-a-proof-id"},
	} {
		if err := consume(tc.user, tc.session, tc.nonce, tc.id, true); !errors.Is(err, ErrIdentityRejected) {
			t.Fatalf("foreign proof accepted or wrong error: %v", err)
		}
	}
	if err := consume(f.student.ID, f.session.ID, nonceHash, proof.ID, false); err != nil {
		t.Fatalf("proof rollback: %v", err)
	}
	if err := consume(f.student.ID, f.session.ID, nonceHash, proof.ID, true); err != nil {
		t.Fatalf("rolled back consumption burned proof: %v", err)
	}
	if err := consume(f.student.ID, f.session.ID, nonceHash, proof.ID, true); !errors.Is(err, ErrIdentityRejected) {
		t.Fatalf("proof replay: %v", err)
	}
}

func TestIdentityCredentialRevocationAndAdministratorReset(t *testing.T) {
	for _, operation := range []string{"revoke", "reset", "expiry"} {
		t.Run(operation, func(t *testing.T) {
			f, credential, proof, nonceHash, _ := identityProofFixture(t)
			s, ctx := f.store, context.Background()
			if err := s.RevokeIdentityCredential(ctx, f.stranger, "attendance.test", credential.ID); err == nil {
				t.Fatal("stranger revoked credential")
			}
			if err := s.ResetStudentIdentity(ctx, f.lecturer, f.student.ID, "attendance.test", "Lost phone"); err == nil {
				t.Fatal("lecturer reset student identity")
			}
			if err := s.ResetStudentIdentity(ctx, f.admin, f.student.ID, "attendance.test", ""); !errors.Is(err, ErrAttendanceInvalid) {
				t.Fatalf("reset without reason: %v", err)
			}
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			id, err := s.BeginIdentityCeremonyTx(ctx, tx, f.student.ID, "attendance.test", auth.HashToken("browser session"), "registration", "", "", []byte(`{"challenge":"test"}`))
			if err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "revoke":
				if err := s.RevokeIdentityCredential(ctx, f.student, "attendance.test", credential.ID); err != nil {
					t.Fatal(err)
				}
			case "reset":
				if err := s.ResetStudentIdentity(ctx, f.admin, f.student.ID, "attendance.test", "Institution checked student identity after loss"); err != nil {
					t.Fatal(err)
				}
				var used bool
				if err := s.Pool.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM identity_ceremonies WHERE id=$1`, id).Scan(&used); err != nil {
					t.Fatal(err)
				}
				if !used {
					t.Fatal("admin reset left ceremony active")
				}
			case "expiry":
				mustExec(t, s, `UPDATE identity_proofs SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, proof.ID)
			}
			tx, err = s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := s.ConsumeIdentityProofTx(ctx, tx, f.student.ID, f.session.ID, nonceHash, proof.ID); !errors.Is(err, ErrIdentityRejected) {
				t.Fatalf("%s proof still accepted: %v", operation, err)
			}
			var auditCount int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action=$1`, map[string]string{"revoke": "identity.credential.revoke", "reset": "identity.student.reset", "expiry": "identity.attendance.confirm"}[operation]).Scan(&auditCount); err != nil {
				t.Fatal(err)
			}
			if auditCount != 1 {
				t.Fatalf("%s audit events=%d", operation, auditCount)
			}
		})
	}
}

func TestIdentityProofAtomicAttendanceSubmission(t *testing.T) {
	for _, mode := range []string{"verified", "required_missing", "revoked", "wrong_rp"} {
		t.Run(mode, func(t *testing.T) {
			f, credential, proof, _, nonce := identityProofFixture(t)
			s, ctx := f.store, context.Background()
			challenge, err := s.IssueAttendanceChallenge(ctx, f.lecturer, f.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			input := AttendanceSubmission{RequireIdentity: true, IdentityProofID: proof.ID, ExpectedRPID: "attendance.test"}
			if mode == "wrong_rp" {
				input.ExpectedRPID = "other.test"
			}
			if mode == "required_missing" {
				input.IdentityProofID = ""
			}
			if mode == "revoked" {
				if err := s.RevokeIdentityCredential(ctx, f.student, "attendance.test", credential.ID); err != nil {
					t.Fatal(err)
				}
			}
			decision, err := s.VerifyAttendanceWithEvidence(ctx, f.student, f.session.ID, challenge.Token, nonce, input)
			if err != nil {
				t.Fatal(err)
			}
			var used, linked bool
			if err := s.Pool.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM identity_proofs WHERE id=$1`, proof.ID).Scan(&used); err != nil {
				t.Fatal(err)
			}
			if err := s.Pool.QueryRow(ctx, `SELECT identity_proof_id IS NOT NULL FROM verification_attempts WHERE session_id=$1 AND student_id=$2`, f.session.ID, f.student.ID).Scan(&linked); err != nil {
				t.Fatal(err)
			}
			if mode == "verified" {
				if decision.Status != "pending" || !used || !linked {
					t.Fatalf("verified decision=%+v used=%v linked=%v", decision, used, linked)
				}
				fresh, err := s.Nonce(ctx, f.student, f.session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if replay, err := s.VerifyAttendanceWithEvidence(ctx, f.student, f.session.ID, challenge.Token, fresh.Nonce, input); err != nil || replay.ReasonCode != "identity_rejected" {
					t.Fatalf("replayed proof decision=%+v err=%v", replay, err)
				}
				var recordCount int
				if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE session_id=$1 AND student_id=$2 AND status='pending'`, f.session.ID, f.student.ID).Scan(&recordCount); err != nil {
					t.Fatal(err)
				}
				if recordCount != 1 {
					t.Fatalf("rejected replay damaged existing record count=%d", recordCount)
				}
			} else {
				want := map[string]string{"required_missing": "identity_required", "revoked": "identity_rejected", "wrong_rp": "identity_rejected"}[mode]
				if decision.Status != "rejected" || decision.ReasonCode != want || used || linked {
					t.Fatalf("%s decision=%+v used=%v linked=%v", mode, decision, used, linked)
				}
				var recordCount int
				if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records`).Scan(&recordCount); err != nil {
					t.Fatal(err)
				}
				if recordCount != 0 {
					t.Fatal("unverified identity created attendance")
				}
			}
		})
	}
}

func TestIdentitySessionRequirementSurvivesUnconfiguredVerifier(t *testing.T) {
	for _, configuredAction := range []string{"create", "open", "challenge"} {
		t.Run(configuredAction, func(t *testing.T) {
			f, ctx := attendanceFixture(t), context.Background()
			s := f.store
			var err error
			if configuredAction == "create" {
				f.session, err = s.CreateAttendanceSession(ctx, f.lecturer, f.course.ID, f.room.ID, "Protected identity session", 10, true)
				if err != nil {
					t.Fatal(err)
				}
				if !f.session.IdentityRequired {
					t.Fatal("configured creation did not persist identity requirement")
				}
			}
			if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID, configuredAction == "open"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.IssueAttendanceChallenge(ctx, f.lecturer, f.session.ID, configuredAction == "challenge"); err != nil {
				t.Fatal(err)
			}
			// A second app or a later startup with identity configuration absent
			// can rotate the challenge but cannot lower this session's policy.
			challenge, err := s.IssueAttendanceChallenge(ctx, f.lecturer, f.session.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := s.AttendanceSession(ctx, f.student, f.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !loaded.IdentityRequired {
				t.Fatal("unconfigured challenge rotation downgraded session identity")
			}
			if err := s.OpenAttendanceSession(ctx, f.lecturer, f.session.ID, false); !errors.Is(err, ErrAttendanceState) {
				t.Fatalf("reopen error=%v", err)
			}
			loaded, err = s.AttendanceSession(ctx, f.student, f.session.ID)
			if err != nil || !loaded.IdentityRequired {
				t.Fatalf("open request downgraded protected policy: session=%+v err=%v", loaded, err)
			}
			for _, legacy := range []bool{false, true} {
				nonce, err := s.Nonce(ctx, f.student, f.session.ID)
				if err != nil {
					t.Fatal(err)
				}
				var decision AttendanceDecision
				if legacy {
					decision, err = s.VerifyAttendance(ctx, f.student, f.session.ID, challenge.Token, nonce.Nonce)
				} else {
					decision, err = s.VerifyAttendanceWithEvidence(ctx, f.student, f.session.ID, challenge.Token, nonce.Nonce, AttendanceSubmission{RequireIdentity: false})
				}
				if err != nil || decision.Status != "rejected" || decision.ReasonCode != "identity_required" {
					t.Fatalf("unconfigured verifier bypassed policy legacy=%v decision=%+v err=%v", legacy, decision, err)
				}
			}
			var records int
			if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM attendance_records WHERE session_id=$1`, f.session.ID).Scan(&records); err != nil {
				t.Fatal(err)
			}
			if records != 0 {
				t.Fatal("unconfigured verifier created attendance for protected session")
			}
		})
	}
}
