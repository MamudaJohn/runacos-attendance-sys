package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func readCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil {
		t.Fatal("missing certificate PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestGenerateTrustChainAndPrivatePermissions(t *testing.T) {
	out := filepath.Join(t.TempDir(), "certs")
	now := time.Now().UTC().Truncate(time.Second)
	const host = "cabv.phone.test"
	if err := generate(host, out, now); err != nil {
		t.Fatal(err)
	}
	root := readCertificate(t, filepath.Join(out, "root-ca.crt"))
	server := readCertificate(t, filepath.Join(out, "server.crt"))
	trust := x509.NewCertPool()
	trust.AddCert(root)
	if _, err := server.Verify(x509.VerifyOptions{Roots: trust, DNSName: host, CurrentTime: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: trust, DNSName: "other.phone.test", CurrentTime: now}); err == nil {
		t.Fatal("accepted wrong DNS hostname")
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: trust, DNSName: host, CurrentTime: now.Add(91 * 24 * time.Hour)}); err == nil {
		t.Fatal("accepted expired server certificate")
	}
	if server.NotBefore != now.Add(-5*time.Minute) || server.NotAfter != now.Add(90*24*time.Hour) {
		t.Fatal("incorrect server validity period")
	}
	if root.NotAfter != now.Add(365*24*time.Hour) {
		t.Fatal("incorrect CA validity period")
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(out, "server.crt"), filepath.Join(out, "server.key")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"root-ca.key", "server.key"} {
			info, err := os.Stat(filepath.Join(out, name))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("%s permission=%o", name, info.Mode().Perm())
			}
		}
	}
}

func TestExistingCAPreservedAndHostnameRenewedWithSameKey(t *testing.T) {
	out := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := generate("first.phone.test", out, now); err != nil {
		t.Fatal(err)
	}
	original := make(map[string][]byte)
	for _, name := range []string{"root-ca.crt", "root-ca.key", "server.key"} {
		content, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = content
	}
	if err := generate("second.phone.test", out, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, before := range original {
		after, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("overwrote %s", name)
		}
	}
	server := readCertificate(t, filepath.Join(out, "server.crt"))
	if err := server.VerifyHostname("second.phone.test"); err != nil {
		t.Fatal(err)
	}
	if err := server.VerifyHostname("first.phone.test"); err == nil {
		t.Fatal("retained old hostname")
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(out, "server.crt"), filepath.Join(out, "server.key")); err != nil {
		t.Fatal(err)
	}
}

func TestRejectInvalidHostAndDoNotWrite(t *testing.T) {
	for _, host := range []string{"", "10.218.25.223", "127.0.0.1", "::1", "https://cabv.test", "cabv.test:8443", "*.cabv.test", "bad_host.test", "bad..test", "-bad.test", "bad-.test", "cabv.test.", "cabv/test"} {
		t.Run(host, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "missing")
			if err := generate(host, out, time.Now()); err == nil {
				t.Fatal("accepted invalid host")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("invalid host created files")
			}
		})
	}
}

func TestIncompleteOrMismatchedCAIsNotOverwritten(t *testing.T) {
	out := t.TempDir()
	now := time.Now()
	if err := os.WriteFile(filepath.Join(out, "root-ca.key"), []byte("private existing material"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := generate("cabv.phone.test", out, now); err == nil {
		t.Fatal("overwrote incomplete CA")
	}
	if value, _ := os.ReadFile(filepath.Join(out, "root-ca.key")); string(value) != "private existing material" {
		t.Fatal("changed existing key")
	}
	a, b := t.TempDir(), t.TempDir()
	if err := generate("cabv.phone.test", a, now); err != nil {
		t.Fatal(err)
	}
	if err := generate("cabv.phone.test", b, now); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(b, "root-ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "root-ca.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := generate("cabv.phone.test", a, now); err == nil {
		t.Fatal("accepted mismatched root certificate/key")
	}
	if value, _ := os.ReadFile(filepath.Join(a, "root-ca.key")); !bytes.Equal(value, key) {
		t.Fatal("changed mismatched key")
	}
}

func TestCAExpiryAndServerWithoutKeyFailWithoutClobber(t *testing.T) {
	out := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := generate("cabv.phone.test", out, now); err != nil {
		t.Fatal(err)
	}
	if err := generate("cabv.phone.test", out, now.Add(365*24*time.Hour)); err == nil {
		t.Fatal("accepted expired CA")
	}
	if err := os.Remove(filepath.Join(out, "server.key")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(out, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := generate("cabv.phone.test", out, now); err == nil {
		t.Fatal("replaced server certificate without original key")
	}
	after, err := os.ReadFile(filepath.Join(out, "server.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("changed server certificate")
	}
}

func TestRunOutputHasURLAndNoPrivateKey(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"-host", "cabv.phone.test", "-out", t.TempDir()}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "https://cabv.phone.test:8443") || !strings.Contains(output.String(), "root-ca.crt") {
		t.Fatal("missing setup guidance")
	}
	if strings.Contains(output.String(), "BEGIN PRIVATE KEY") {
		t.Fatal("printed secret key")
	}
}

func TestSymlinkCertificateRejected(t *testing.T) {
	out := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(out, "root-ca.key")); err != nil {
		t.Skip("symlink creation unavailable", err)
	}
	if err := generate("cabv.phone.test", out, time.Now()); err == nil {
		t.Fatal("accepted symlink key")
	}
	if data, _ := os.ReadFile(target); string(data) != "do not change" {
		t.Fatal("modified symlink target")
	}
}
