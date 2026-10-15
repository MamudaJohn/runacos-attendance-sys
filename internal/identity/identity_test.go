package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// A software authenticator exercises the same CBOR, public key and signed
// assertion format sent by a browser. It does not model physical biometrics.
type testAuthenticator struct {
	key *ecdsa.PrivateKey
	id  []byte
}

func newTestAuthenticator(t *testing.T) testAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	return testAuthenticator{key: key, id: id}
}

func encoded(v []byte) string { return base64.RawURLEncoding.EncodeToString(v) }

func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func optionChallenge(t *testing.T, options CeremonyOptions) string {
	t.Helper()
	var fields struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(jsonBytes(t, options.PublicKey), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Challenge == "" {
		t.Fatal("empty authenticator challenge")
	}
	return fields.Challenge
}

func (a testAuthenticator) registration(t *testing.T, challenge, origin, rpID string, flags byte) []byte {
	t.Helper()
	publicKey, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1,
		-2: a.key.X.FillBytes(make([]byte, 32)), -3: a.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	rpHash := sha256.Sum256([]byte(rpID))
	data := append([]byte{}, rpHash[:]...)
	data = append(data, flags)
	data = append(data, 0, 0, 0, 0)          // Signature counter starts at zero.
	data = append(data, make([]byte, 16)...) // None attestation, anonymous AAGUID.
	data = binary.BigEndian.AppendUint16(data, uint16(len(a.id)))
	data = append(data, a.id...)
	data = append(data, publicKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "authData": data, "attStmt": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	client := jsonBytes(t, map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin, "crossOrigin": false})
	return jsonBytes(t, map[string]any{"id": encoded(a.id), "rawId": encoded(a.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response":               map[string]any{"clientDataJSON": encoded(client), "attestationObject": encoded(attestation), "transports": []string{"internal"}},
		"clientExtensionResults": map[string]any{}})
}

func (a testAuthenticator) assertion(t *testing.T, challenge, origin, rpID, userID string, flags byte, counter uint32, corruptSignature bool) []byte {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	data := append([]byte{}, rpHash[:]...)
	data = append(data, flags)
	data = binary.BigEndian.AppendUint32(data, counter)
	client := jsonBytes(t, map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin, "crossOrigin": false})
	clientHash := sha256.Sum256(client)
	digest := sha256.Sum256(append(append([]byte{}, data...), clientHash[:]...))
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if corruptSignature {
		signature[len(signature)-1] ^= 1
	}
	return jsonBytes(t, map[string]any{"id": encoded(a.id), "rawId": encoded(a.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response":               map[string]any{"clientDataJSON": encoded(client), "authenticatorData": encoded(data), "signature": encoded(signature), "userHandle": encoded([]byte(userID))},
		"clientExtensionResults": map[string]any{}})
}

type identityFixture struct {
	s                     *store.Store
	service               *Service
	student, other, admin store.User
	session               store.AttendanceSession
	authSessionHash       string
}

func identityDatabase(t *testing.T) *store.Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; requires disposable PostgreSQL")
	}
	ctx := context.Background()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "cabv_identity_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	defer db.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(ctx, db, filepath.Join("..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

func fixture(t *testing.T) identityFixture {
	t.Helper()
	s, ctx := identityDatabase(t), context.Background()
	user := func(email, role string) store.User {
		var u store.User
		if err := s.Pool.QueryRow(ctx, `INSERT INTO users(email,name,role,password_hash) VALUES($1,$1,$2,'test-hash') RETURNING id::text,email,name,role`, email, role).Scan(&u.ID, &u.Email, &u.Name, &u.Role); err != nil {
			t.Fatal(err)
		}
		return u
	}
	f := identityFixture{s: s, student: user("student@test.example", "student"), other: user("other@test.example", "student"), admin: user("admin@test.example", "admin"), authSessionHash: auth.HashToken("authenticated browser session")}
	var courseID string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO courses(code,title) VALUES('ID101','Identity tests') RETURNING id::text`).Scan(&courseID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO enrollments(course_id,student_id) VALUES($1,$2),($1,$3)`, courseID, f.student.ID, f.other.ID); err != nil {
		t.Fatal(err)
	}
	room, err := s.SaveRoom(ctx, f.admin, "", "Identity room", "room", 40)
	if err != nil {
		t.Fatal(err)
	}
	f.session, err = s.CreateAttendanceSession(ctx, f.admin, courseID, room.ID, "Identity lecture", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.OpenAttendanceSession(ctx, f.admin, f.session.ID); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(s, "attendance.test", "https://attendance.test")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f identityFixture) register(t *testing.T, a testAuthenticator) store.IdentityCredential {
	t.Helper()
	options, err := f.service.BeginRegistration(context.Background(), f.student, f.authSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	var selection struct {
		AuthenticatorSelection struct {
			Attachment string `json:"authenticatorAttachment"`
			UV         string `json:"userVerification"`
		} `json:"authenticatorSelection"`
	}
	if err := json.Unmarshal(jsonBytes(t, options.PublicKey), &selection); err != nil {
		t.Fatal(err)
	}
	if selection.AuthenticatorSelection.Attachment != "platform" || selection.AuthenticatorSelection.UV != "required" {
		t.Fatalf("registration selection=%+v", selection)
	}
	credential, err := f.service.FinishRegistration(context.Background(), f.student, f.authSessionHash, options.TransactionID, a.registration(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", 0x45))
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestIdentityOriginConfiguration(t *testing.T) {
	for _, tc := range []struct {
		rpID, origin string
		valid        bool
	}{
		{"attendance.test", "https://attendance.test", true}, {"localhost", "http://localhost:8080", true},
		{"attendance.test", "http://attendance.test", false}, {"127.0.0.1", "https://127.0.0.1", false},
		{"attendance.test", "https://other.test", false}, {"Attendance.test", "https://Attendance.test", false},
		{"attendance.test", "https://attendance.test/", false}, {"attendance.test", "https://attendance.test?query=yes", false},
		{"attendance.test", "https://user:password@attendance.test", false}, {"attendance.test", "https://attendance.test#fragment", false},
		{"", "https://attendance.test", false}, {"attendance.test", "attendance.test", false},
	} {
		s, err := New(&store.Store{}, tc.rpID, tc.origin)
		if (err == nil) != tc.valid {
			t.Errorf("RP=%q origin=%q service=%v err=%v", tc.rpID, tc.origin, s, err)
		}
	}
	if _, err := New(nil, "attendance.test", "https://attendance.test"); err == nil {
		t.Fatal("nil database accepted")
	}
}

func TestIdentitySignedRegistrationAndAttendance(t *testing.T) {
	f, ctx := fixture(t), context.Background()
	a := newTestAuthenticator(t)
	credential := f.register(t, a)
	if credential.ID == "" {
		t.Fatal("empty saved credential")
	}
	credentials, err := f.service.Credentials(ctx, f.student)
	if err != nil || len(credentials) != 1 {
		t.Fatalf("credentials=%v err=%v", credentials, err)
	}
	var saved webauthn.Credential
	if err := json.Unmarshal(credentials[0].Data, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Flags.UserVerified || saved.Authenticator.SignCount != 0 || saved.Authenticator.Attachment != "platform" {
		t.Fatalf("saved credential=%+v", saved)
	}
	nonce, err := f.s.Nonce(ctx, f.student, f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	options, err := f.service.BeginAttendance(ctx, f.student, f.session.ID, nonce.Nonce, f.authSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	response := a.assertion(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", f.student.ID, 0x05, 1, false)
	proof, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, f.authSessionHash, options.TransactionID, response)
	if err != nil || proof.ID == "" || !proof.ExpiresAt.After(time.Now()) {
		t.Fatalf("proof=%+v err=%v", proof, err)
	}
	if _, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, f.authSessionHash, options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("replayed ceremony err=%v", err)
	}
	credentials, err = f.service.Credentials(ctx, f.student)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(credentials[0].Data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Authenticator.SignCount != 1 || credentials[0].LastUsedAt == nil {
		t.Fatal("successful assertion did not persist counter and use time")
	}
	// A correctly signed assertion whose nonzero counter fails to advance is
	// rejected rather than overwriting the stored counter or generating a proof.
	options, err = f.service.BeginAttendance(ctx, f.student, f.session.ID, nonce.Nonce, f.authSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, f.authSessionHash, options.TransactionID, a.assertion(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", f.student.ID, 0x05, 1, false)); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("counter rollback err=%v", err)
	}
	var proofCount int
	if err := f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM identity_proofs`).Scan(&proofCount); err != nil {
		t.Fatal(err)
	}
	if proofCount != 1 {
		t.Fatalf("proof count=%d", proofCount)
	}
}

func TestIdentityRejectsInvalidRegistration(t *testing.T) {
	for _, kind := range []string{"origin", "rp", "challenge", "uv", "presence"} {
		t.Run(kind, func(t *testing.T) {
			f, ctx := fixture(t), context.Background()
			a := newTestAuthenticator(t)
			options, err := f.service.BeginRegistration(ctx, f.student, f.authSessionHash)
			if err != nil {
				t.Fatal(err)
			}
			challenge, origin, rpID, flags := optionChallenge(t, options), "https://attendance.test", "attendance.test", byte(0x45)
			switch kind {
			case "origin":
				origin = "https://attacker.test"
			case "rp":
				rpID = "attacker.test"
			case "challenge":
				challenge = encoded([]byte("another challenge"))
			case "uv":
				flags = 0x41
			case "presence":
				flags = 0x44
			}
			if _, err := f.service.FinishRegistration(ctx, f.student, f.authSessionHash, options.TransactionID, a.registration(t, challenge, origin, rpID, flags)); !errors.Is(err, store.ErrIdentityRejected) {
				t.Fatalf("invalid %s err=%v", kind, err)
			}
			if _, err := f.service.FinishRegistration(ctx, f.student, f.authSessionHash, options.TransactionID, a.registration(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", 0x45)); !errors.Is(err, store.ErrIdentityRejected) {
				t.Fatalf("failed ceremony remained reusable err=%v", err)
			}
			rows, err := f.service.Credentials(ctx, f.student)
			if err != nil || len(rows) != 0 {
				t.Fatalf("invalid registration persisted credentials=%v err=%v", rows, err)
			}
		})
	}
}

func TestIdentityRejectsInvalidSignedAssertions(t *testing.T) {
	for _, kind := range []string{"signature", "unknown_credential", "origin", "rp", "challenge", "uv", "presence", "user_handle", "expired_ceremony", "expired_nonce", "used_nonce", "replaced_nonce", "closed_session", "withdrawn"} {
		t.Run(kind, func(t *testing.T) {
			f, ctx := fixture(t), context.Background()
			a := newTestAuthenticator(t)
			f.register(t, a)
			nonce, err := f.s.Nonce(ctx, f.student, f.session.ID)
			if err != nil {
				t.Fatal(err)
			}
			options, err := f.service.BeginAttendance(ctx, f.student, f.session.ID, nonce.Nonce, f.authSessionHash)
			if err != nil {
				t.Fatal(err)
			}
			challenge, origin, rpID, handle, flags := optionChallenge(t, options), "https://attendance.test", "attendance.test", f.student.ID, byte(0x05)
			switch kind {
			case "unknown_credential":
				a = newTestAuthenticator(t)
			case "origin":
				origin = "https://attacker.test"
			case "rp":
				rpID = "attacker.test"
			case "challenge":
				challenge = encoded([]byte("different challenge"))
			case "uv":
				flags = 0x01
			case "presence":
				flags = 0x04
			case "user_handle":
				handle = f.other.ID
			case "expired_ceremony":
				_, err = f.s.Pool.Exec(ctx, `UPDATE identity_ceremonies SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, options.TransactionID)
			case "expired_nonce":
				_, err = f.s.Pool.Exec(ctx, `UPDATE submission_nonces SET expires_at=clock_timestamp()-interval '1 second' WHERE nonce_hash=$1`, auth.HashToken(nonce.Nonce))
			case "used_nonce":
				_, err = f.s.Pool.Exec(ctx, `UPDATE submission_nonces SET used_at=clock_timestamp() WHERE nonce_hash=$1`, auth.HashToken(nonce.Nonce))
			case "replaced_nonce":
				_, err = f.s.Nonce(ctx, f.student, f.session.ID)
			case "closed_session":
				err = f.s.CloseAttendanceSession(ctx, f.admin, f.session.ID)
			case "withdrawn":
				_, err = f.s.Pool.Exec(ctx, `UPDATE enrollments SET status='withdrawn' WHERE student_id=$1`, f.student.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, f.authSessionHash, options.TransactionID, a.assertion(t, challenge, origin, rpID, handle, flags, 1, kind == "signature")); err == nil {
				t.Fatalf("invalid %s accepted", kind)
			}
			var count int
			if err := f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM identity_proofs`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("invalid %s created %d proofs", kind, count)
			}
		})
	}
}

func TestIdentityAttendanceCeremonyScope(t *testing.T) {
	f, ctx := fixture(t), context.Background()
	a := newTestAuthenticator(t)
	f.register(t, a)
	nonce, err := f.s.Nonce(ctx, f.student, f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	options, err := f.service.BeginAttendance(ctx, f.student, f.session.ID, nonce.Nonce, f.authSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	response := a.assertion(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", f.student.ID, 0x05, 1, false)
	if _, err := f.service.FinishAttendance(ctx, f.other, f.session.ID, f.authSessionHash, options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("wrong attendance owner: %v", err)
	}
	if _, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, auth.HashToken("other browser"), options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("wrong attendance browser: %v", err)
	}
	otherSession, err := f.s.CreateAttendanceSession(ctx, f.admin, f.session.CourseID, f.session.RoomID, "Other lecture", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.OpenAttendanceSession(ctx, f.admin, otherSession.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FinishAttendance(ctx, f.student, otherSession.ID, f.authSessionHash, options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("wrong attendance session: %v", err)
	}
	if _, err := f.service.FinishAttendance(ctx, f.student, f.session.ID, f.authSessionHash, options.TransactionID, response); err != nil {
		t.Fatalf("foreign requests consumed rightful ceremony: %v", err)
	}
}

func TestIdentityCeremonyOwnerAndBrowserBinding(t *testing.T) {
	f, ctx := fixture(t), context.Background()
	a := newTestAuthenticator(t)
	options, err := f.service.BeginRegistration(ctx, f.student, f.authSessionHash)
	if err != nil {
		t.Fatal(err)
	}
	response := a.registration(t, optionChallenge(t, options), "https://attendance.test", "attendance.test", 0x45)
	if _, err := f.service.FinishRegistration(ctx, f.other, f.authSessionHash, options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("wrong owner err=%v", err)
	}
	if _, err := f.service.FinishRegistration(ctx, f.student, auth.HashToken("another browser session"), options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("wrong browser err=%v", err)
	}
	if _, err := f.service.FinishRegistration(ctx, f.student, f.authSessionHash, options.TransactionID, response); err != nil {
		t.Fatalf("foreign attempts consumed owner's ceremony: %v", err)
	}
	if _, err := f.service.FinishRegistration(ctx, f.student, f.authSessionHash, options.TransactionID, response); !errors.Is(err, store.ErrIdentityRejected) {
		t.Fatalf("registration replay err=%v", err)
	}
	if _, err := f.service.BeginRegistration(ctx, f.admin, f.authSessionHash); err == nil {
		t.Fatal("admin registered student credential")
	}
	if _, err := f.service.BeginAttendance(ctx, f.other, f.session.ID, "not-a-token", f.authSessionHash); err == nil {
		t.Fatal("invalid nonce accepted")
	}
}
