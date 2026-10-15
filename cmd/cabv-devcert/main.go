// cabv-devcert creates an explicit, local-only trust root for HTTPS phone tests.
// It never installs the CA in an operating-system or browser trust store.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "local HTTPS certificate:", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("cabv-devcert", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	host := flags.String("host", "", "Exact DNS hostname used by both phone and laptop")
	out := flags.String("out", ".local/tls", "Private local certificate directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err := generate(*host, *out, time.Now()); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "Local HTTPS certificates ready for https://%s:8443\nPublic phone trust certificate: %s\nInstall this public CA certificate on your test device manually. Keep all .key files private.\n", *host, filepath.Join(absolute, "root-ca.crt"))
	return nil
}

func validateHostname(host string) error {
	if host == "" || len(host) > 253 || net.ParseIP(host) != nil {
		return errors.New("-host must be an exact DNS hostname, not an IP address")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid DNS hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("invalid DNS hostname; omit scheme, port, path and wildcards")
			}
		}
	}
	return nil
}

func generate(host, out string, now time.Time) error {
	if err := validateHostname(host); err != nil {
		return err
	}
	if out == "" {
		return errors.New("certificate directory is empty")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	info, err := os.Lstat(out)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("certificate directory must be a regular directory, not a symlink")
	}
	root, err := os.OpenRoot(out)
	if err != nil {
		return fmt.Errorf("open certificate directory: %w", err)
	}
	defer root.Close()
	for _, name := range []string{"root-ca.crt", "root-ca.key", "server.crt", "server.key"} {
		if info, err := root.Lstat(name); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s must be a regular file, not a symlink", name)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	ca, caKey, err := loadOrCreateCA(root, now)
	if err != nil {
		return err
	}
	serverKey, err := loadOrCreateServerKey(root)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	notAfter := now.Add(90 * 24 * time.Hour)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host, Organization: []string{"CABV local phone test"}},
		DNSNames: []string{host}, NotBefore: now.Add(-5 * time.Minute), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("sign local server certificate: %w", err)
	}
	return writeAtomic(root, "server.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

func loadOrCreateCA(root *os.Root, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, certErr := root.ReadFile("root-ca.crt")
	keyPEM, keyErr := root.ReadFile("root-ca.key")
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		serial, err := randomSerial()
		if err != nil {
			return nil, nil, err
		}
		template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "CABV local phone-test CA"},
			NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(365 * 24 * time.Hour),
			IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			return nil, nil, err
		}
		encodedKey, err := encodePrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		// O_EXCL prevents replacing a trust root during a concurrent invocation.
		if err := writeExclusive(root, "root-ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
			return nil, nil, fmt.Errorf("create root certificate without overwrite: %w", err)
		}
		if err := writeExclusive(root, "root-ca.key", encodedKey, 0600); err != nil {
			// Remove only the certificate this invocation just created.
			root.Remove("root-ca.crt")
			return nil, nil, fmt.Errorf("create private root key without overwrite: %w", err)
		}
		cert, err := x509.ParseCertificate(der)
		return cert, key, err
	}
	if certErr != nil || keyErr != nil {
		return nil, nil, errors.New("local CA is incomplete or unreadable; use a new output directory rather than overwrite existing trust material")
	}
	certBlock, rest := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errors.New("invalid root CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, errors.New("invalid root CA certificate")
	}
	key, err := decodePrivateKey(keyPEM)
	if err != nil {
		return nil, nil, err
	}
	public, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&key.PublicKey) || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil {
		return nil, nil, errors.New("root CA certificate and key do not form a valid self-signed CA")
	}
	if now.Before(cert.NotBefore) || !cert.NotAfter.After(now.Add(24*time.Hour)) {
		return nil, nil, errors.New("root CA is not currently valid for at least another day; create a new CA in a new directory")
	}
	return cert, key, nil
}

func loadOrCreateServerKey(root *os.Root) (*ecdsa.PrivateKey, error) {
	encoded, err := root.ReadFile("server.key")
	if err == nil {
		// Reusing this validated private key allows certificate renewal/hostname
		// changes without a two-file key/certificate replacement window.
		return decodePrivateKey(encoded)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot read existing server private key")
	}
	if _, err := root.Lstat("server.crt"); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("server certificate exists without its private key; use a new directory")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	encoded, err = encodePrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(root, "server.key", encoded, 0600); err != nil {
		return nil, fmt.Errorf("create server key without overwrite: %w", err)
	}
	return key, nil
}

func encodePrivateKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func decodePrivateKey(encoded []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid private key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS8 private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() || key.D == nil || key.D.Sign() <= 0 || !key.Curve.IsOnCurve(key.X, key.Y) {
		return nil, errors.New("private key must be a valid ECDSA P-256 key")
	}
	return key, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err == nil && serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, err
}

func writeExclusive(root *os.Root, name string, data []byte, mode os.FileMode) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		root.Remove(name)
	}
	return err
}

func writeAtomic(root *os.Root, name string, data []byte, mode os.FileMode) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	temporary := name + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := writeExclusive(root, temporary, data, mode); err != nil {
		return err
	}
	defer root.Remove(temporary)
	if err := root.Rename(temporary, name); err != nil {
		return fmt.Errorf("replace server certificate: %w", err)
	}
	return nil
}
