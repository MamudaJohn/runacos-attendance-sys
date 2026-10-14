package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestPasswords(t *testing.T) {
	password := "a long password with spaces"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first[len(passwordPrefix):len(passwordPrefix)+passwordSaltChars] == second[len(passwordPrefix):len(passwordPrefix)+passwordSaltChars] {
		t.Fatal("hashes must use independent random salts")
	}
	if !VerifyPassword(first, password) || VerifyPassword(first, "a different password") {
		t.Fatal("password verification failed")
	}
	if VerifyPassword(first, " "+password) || VerifyPassword(first, strings.Repeat("x", MaxPasswordBytes+1)) {
		t.Fatal("verification changed or accepted an oversized password")
	}
	fields := strings.Split(first, "$")
	salt, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil {
		t.Fatal(err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 600_000, 32)
	if err != nil || fields[4] != base64.RawStdEncoding.EncodeToString(key) {
		t.Fatal("hash does not use the expected PBKDF2 parameters")
	}
}

func TestPasswordLength(t *testing.T) {
	for _, password := range []string{"", "short", strings.Repeat("x", MinPasswordBytes-1), strings.Repeat("x", MaxPasswordBytes+1), strings.Repeat("é", 65)} {
		if _, err := HashPassword(password); !errors.Is(err, ErrPasswordLength) {
			t.Errorf("length %d: got %v", len(password), err)
		}
	}
	for _, password := range []string{strings.Repeat("x", MinPasswordBytes), strings.Repeat("x", MaxPasswordBytes), strings.Repeat("é", 6)} {
		encoded, err := HashPassword(password)
		if err != nil || !VerifyPassword(encoded, password) {
			t.Errorf("valid byte length %d rejected: %v", len(password), err)
		}
	}
}

func TestRejectMalformedPasswordHashes(t *testing.T) {
	encoded, err := HashPassword("valid long password")
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		"", encoded + "$extra", encoded[:len(encoded)-1],
		strings.Replace(encoded, "600000", "999999", 1),
		strings.Replace(encoded, "600000", "000001", 1),
		strings.Replace(encoded, "sha256", "sha512", 1),
		encoded[:len(passwordPrefix)] + "!" + encoded[len(passwordPrefix)+1:],
		encoded[:len(passwordPrefix)+passwordSaltChars] + ":" + encoded[len(passwordPrefix)+passwordSaltChars+1:],
		encoded[:len(encoded)-1] + "!",
		strings.Repeat("x", 1<<20),
	} {
		if VerifyPassword(invalid, "valid long password") {
			t.Error("accepted a malformed hash")
		}
	}
}

func TestTokensAndCSRF(t *testing.T) {
	first, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(raw) != 32 || len(first) != 43 || first == second {
		t.Fatal("expected distinct 256-bit URL-safe tokens")
	}
	if HashToken("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("incorrect token digest")
	}
	if HashToken(first) == first || HashToken(first) == HashToken(second) || CSRF(first) == HashToken(first) {
		t.Fatal("token or CSRF digest is not separated")
	}
	if CSRF(first) != CSRF(first) || !ValidCSRF(first, CSRF(first)) {
		t.Fatal("valid CSRF token rejected")
	}
	for _, submitted := range []string{"", first, CSRF(second), CSRF(first) + "x", strings.Repeat("x", 64)} {
		if ValidCSRF(first, submitted) {
			t.Fatal("invalid CSRF token accepted")
		}
	}
	if ValidCSRF("", CSRF("")) {
		t.Fatal("empty cookie token accepted")
	}
}
