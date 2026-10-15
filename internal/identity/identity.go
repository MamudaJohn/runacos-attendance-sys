// Package identity provides WebAuthn user verification for attendance. A
// platform authenticator verifies an enrolled credential; its operating system
// chooses fingerprint, face or PIN. This is not institutional identity proof.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"cabv/internal/auth"
	"cabv/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5"
)

type Service struct {
	store        *store.Store
	webauthn     *webauthn.WebAuthn
	origin, rpID string
}

// New deliberately accepts one exact origin and the matching hostname. Phone
// browsers require trusted HTTPS; only localhost HTTP is allowed for developers.
func New(database *store.Store, rpID, origin string) (*Service, error) {
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("identity origin must be an absolute origin without path, credentials, query or fragment")
	}
	if rpID == "" || rpID != u.Hostname() || strings.ToLower(rpID) != rpID || net.ParseIP(rpID) != nil || strings.ContainsAny(rpID, " /:@") {
		return nil, fmt.Errorf("identity RP ID must equal the origin hostname; IP addresses are not supported")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && rpID == "localhost") {
		return nil, fmt.Errorf("identity needs HTTPS or http://localhost")
	}
	if database == nil {
		return nil, fmt.Errorf("identity database is required")
	}
	timeout := webauthn.TimeoutConfig{Enforce: true, Timeout: 5 * time.Minute, TimeoutUVD: 5 * time.Minute}
	w, err := webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: "CABV Attendance", RPOrigins: []string{origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{AuthenticatorAttachment: protocol.Platform, UserVerification: protocol.VerificationRequired, ResidentKey: protocol.ResidentKeyRequirementPreferred},
		AttestationPreference:  protocol.PreferNoAttestation, Timeouts: webauthn.TimeoutsConfig{Login: timeout, Registration: timeout}})
	if err != nil {
		return nil, err
	}
	return &Service{store: database, webauthn: w, origin: origin, rpID: rpID}, nil
}

func (s *Service) Origin() string { return s.origin }
func (s *Service) RPID() string   { return s.rpID }

type CeremonyOptions struct {
	TransactionID string `json:"transaction_id"`
	PublicKey     any    `json:"public_key"`
}

type credentialUser struct {
	store.User
	credentials []webauthn.Credential
}

func (u credentialUser) WebAuthnID() []byte                         { return []byte(u.ID) }
func (u credentialUser) WebAuthnName() string                       { return u.Email }
func (u credentialUser) WebAuthnDisplayName() string                { return u.Name }
func (u credentialUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func (s *Service) userTx(ctx context.Context, tx pgx.Tx, actor store.User, attendanceID string) (credentialUser, []store.IdentityCredential, error) {
	u, err := s.store.LockIdentityActorTx(ctx, tx, actor, attendanceID)
	if err != nil {
		return credentialUser{}, nil, err
	}
	rows, err := s.store.IdentityCredentialsTx(ctx, tx, u.ID, s.rpID)
	if err != nil {
		return credentialUser{}, nil, err
	}
	user := credentialUser{User: u, credentials: make([]webauthn.Credential, 0, len(rows))}
	for _, row := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(row.Data, &c); err != nil {
			return credentialUser{}, nil, err
		}
		user.credentials = append(user.credentials, c)
	}
	return user, rows, nil
}

func (s *Service) Credentials(ctx context.Context, actor store.User) ([]store.IdentityCredential, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	_, rows, err := s.userTx(ctx, tx, actor, "")
	if err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}

func (s *Service) BeginRegistration(ctx context.Context, actor store.User, authSessionHash string) (CeremonyOptions, error) {
	return s.begin(ctx, actor, authSessionHash, "", "")
}
func (s *Service) BeginAttendance(ctx context.Context, actor store.User, attendanceID, nonce, authSessionHash string) (CeremonyOptions, error) {
	if attendanceID == "" || !validToken(nonce) {
		return CeremonyOptions{}, store.ErrIdentityRejected
	}
	return s.begin(ctx, actor, authSessionHash, attendanceID, auth.HashToken(nonce))
}
func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (s *Service) begin(ctx context.Context, actor store.User, authSessionHash, attendanceID, nonceHash string) (CeremonyOptions, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return CeremonyOptions{}, err
	}
	defer tx.Rollback(ctx)
	u, rows, err := s.userTx(ctx, tx, actor, attendanceID)
	if err != nil {
		return CeremonyOptions{}, err
	}
	purpose := "registration"
	var options any
	var session *webauthn.SessionData
	if attendanceID == "" {
		if len(rows) >= 8 {
			return CeremonyOptions{}, store.ErrIdentityLimit
		}
		exclusions := make([]protocol.CredentialDescriptor, 0, len(u.credentials))
		for _, c := range u.credentials {
			exclusions = append(exclusions, c.Descriptor())
		}
		creation, newSession, beginErr := s.webauthn.BeginRegistration(u, webauthn.WithExclusions(exclusions), webauthn.WithRegistrationOrigin(s.origin))
		if beginErr != nil {
			return CeremonyOptions{}, beginErr
		}
		options = creation.Response
		session = newSession
	} else {
		purpose = "attendance"
		if len(rows) == 0 {
			return CeremonyOptions{}, store.ErrIdentityRejected
		}
		if err := s.store.CheckIdentityNonceTx(ctx, tx, u.ID, attendanceID, nonceHash); err != nil {
			return CeremonyOptions{}, err
		}
		assertion, newSession, beginErr := s.webauthn.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired), webauthn.WithLoginOrigin(s.origin))
		if beginErr != nil {
			return CeremonyOptions{}, beginErr
		}
		options = assertion.Response
		session = newSession
	}
	data, err := json.Marshal(session)
	if err != nil {
		return CeremonyOptions{}, err
	}
	id, err := s.store.BeginIdentityCeremonyTx(ctx, tx, u.ID, s.rpID, authSessionHash, purpose, attendanceID, nonceHash, data)
	if err != nil {
		return CeremonyOptions{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CeremonyOptions{}, err
	}
	return CeremonyOptions{TransactionID: id, PublicKey: options}, nil
}

// FinishRegistration checks the actual authenticator attestation with UV
// required; the client cannot replace it with a boolean. Even failed responses
// consume the matching ceremony, while a wrong owner/session cannot consume it.
func (s *Service) FinishRegistration(ctx context.Context, actor store.User, authSessionHash, transactionID string, rawCredential []byte) (store.IdentityCredential, error) {
	credential, _, err := s.finish(ctx, actor, authSessionHash, "", transactionID, rawCredential)
	return credential, err
}
func (s *Service) FinishAttendance(ctx context.Context, actor store.User, attendanceID, authSessionHash, transactionID string, rawCredential []byte) (store.IdentityProof, error) {
	_, proof, err := s.finish(ctx, actor, authSessionHash, attendanceID, transactionID, rawCredential)
	return proof, err
}

func (s *Service) finish(ctx context.Context, actor store.User, authSessionHash, attendanceID, transactionID string, rawCredential []byte) (store.IdentityCredential, store.IdentityProof, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	defer tx.Rollback(ctx)
	u, _, err := s.userTx(ctx, tx, actor, attendanceID)
	if err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	purpose := "registration"
	if attendanceID != "" {
		purpose = "attendance"
	}
	ceremony, err := s.store.LockIdentityCeremonyTx(ctx, tx, u.ID, s.rpID, authSessionHash, purpose, attendanceID, transactionID)
	if err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	if err := s.store.UseIdentityCeremonyTx(ctx, tx, ceremony.ID); err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	reject := func() (store.IdentityCredential, store.IdentityProof, error) {
		if err := tx.Commit(ctx); err != nil {
			return store.IdentityCredential{}, store.IdentityProof{}, err
		}
		return store.IdentityCredential{}, store.IdentityProof{}, store.ErrIdentityRejected
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(ceremony.Data, &session); err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	if session.UserVerification != protocol.VerificationRequired || len(rawCredential) == 0 || len(rawCredential) > 16*1024 {
		return reject()
	}
	var credential *webauthn.Credential
	if attendanceID == "" {
		response, parseErr := protocol.ParseCredentialCreationResponseBytes(rawCredential)
		if parseErr != nil {
			return reject()
		}
		credential, err = s.webauthn.CreateCredential(u, session, response)
	} else {
		if err := s.store.CheckIdentityNonceTx(ctx, tx, u.ID, attendanceID, ceremony.NonceHash); err != nil {
			if errors.Is(err, store.ErrIdentityRejected) {
				return reject()
			}
			return store.IdentityCredential{}, store.IdentityProof{}, err
		}
		response, parseErr := protocol.ParseCredentialRequestResponseBytes(rawCredential)
		if parseErr != nil {
			return reject()
		}
		credential, err = s.webauthn.ValidateLogin(u, session, response)
	}
	if err != nil || credential == nil || credential.Authenticator.CloneWarning {
		return reject()
	}
	encoded, err := json.Marshal(credential)
	if err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	var saved store.IdentityCredential
	var proof store.IdentityProof
	if attendanceID == "" {
		saved, err = s.store.SaveIdentityCredentialTx(ctx, tx, u.ID, s.rpID, credential.ID, encoded)
	} else {
		var id string
		id, err = s.store.UpdateIdentityCredentialTx(ctx, tx, u.ID, s.rpID, credential.ID, encoded)
		if err == nil {
			proof, err = s.store.SaveIdentityProofTx(ctx, tx, u.ID, attendanceID, id, ceremony.NonceHash)
		}
	}
	if err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.IdentityCredential{}, store.IdentityProof{}, err
	}
	return saved, proof, nil
}
