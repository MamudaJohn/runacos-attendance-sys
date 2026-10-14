// Package auth provides password, session-token, and login-rate-limit primitives.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	MinPasswordBytes     = 12
	MaxPasswordBytes     = 128
	passwordIterations   = 600_000
	passwordSaltBytes    = 16
	passwordKeyBytes     = 32
	passwordPrefix       = "$pbkdf2-sha256$600000$"
	passwordSaltChars    = 22
	passwordKeyChars     = 43
	passwordEncodedChars = len(passwordPrefix) + passwordSaltChars + 1 + passwordKeyChars
)

var ErrPasswordLength = errors.New("password must be between 12 and 128 bytes")

// HashPassword creates a salted PBKDF2-HMAC-SHA256 password hash. Length is
// measured in bytes without trimming or otherwise changing the password.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return "", ErrPasswordLength
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	if err != nil {
		return "", err
	}
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword accepts only this application's fixed hash format. The
// iteration count and field sizes cannot be supplied by a caller, preventing
// malformed stored hashes from requesting unbounded CPU or memory.
func VerifyPassword(encoded, password string) bool {
	if len(password) > MaxPasswordBytes || len(encoded) != passwordEncodedChars || !strings.HasPrefix(encoded, passwordPrefix) {
		return false
	}
	fields := encoded[len(passwordPrefix):]
	if fields[passwordSaltChars] != '$' {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(fields[:passwordSaltChars])
	if err != nil || len(salt) != passwordSaltBytes {
		return false
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(fields[passwordSaltChars+1:])
	if err != nil || len(expected) != passwordKeyBytes {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	return err == nil && subtle.ConstantTimeCompare(actual, expected) == 1
}

// Token returns a 256-bit random token suitable for a session cookie.
func Token() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken returns the digest to store instead of the bearer token itself.
func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// CSRF binds a form token to a secret session or pre-login cookie token.
func CSRF(token string) string {
	return HashToken("cabv-csrf:" + token)
}

// ValidCSRF compares the expected form token in constant time. Empty cookie
// tokens are rejected even if their derived form token was submitted.
func ValidCSRF(token, submitted string) bool {
	if token == "" || len(submitted) != sha256.Size*2 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(CSRF(token)), []byte(submitted)) == 1
}
