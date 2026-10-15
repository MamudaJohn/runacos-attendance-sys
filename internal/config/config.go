package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
)

type Config struct {
	HTTPAddr, DatabaseURL string
	SecureCookies         bool
	ResourceDir           string
	ResourceQuotaBytes    int64
	WebAuthnRPID          string
	PublicOrigin          string
	TLSCertFile           string
	TLSKeyFile            string
}

func Load() (Config, error) { return Parse(os.Getenv) }

func Parse(get func(string) string) (Config, error) {
	c := Config{HTTPAddr: get("HTTP_ADDR"), DatabaseURL: get("DATABASE_URL")}
	c.WebAuthnRPID, c.PublicOrigin = get("WEBAUTHN_RP_ID"), get("PUBLIC_ORIGIN")
	c.TLSCertFile, c.TLSKeyFile = get("TLS_CERT_FILE"), get("TLS_KEY_FILE")
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return Config{}, fmt.Errorf("TLS_CERT_FILE and TLS_KEY_FILE must be configured together")
	}
	if (c.WebAuthnRPID == "") != (c.PublicOrigin == "") {
		return Config{}, fmt.Errorf("WEBAUTHN_RP_ID and PUBLIC_ORIGIN must be configured together")
	}
	c.ResourceDir = get("RESOURCE_DIR")
	if c.ResourceDir == "" {
		c.ResourceDir = ".local/resources"
	}
	c.ResourceQuotaBytes = 100 * 1024 * 1024
	if value := get("RESOURCE_QUOTA_BYTES"); value != "" {
		quota, err := strconv.ParseInt(value, 10, 64)
		if err != nil || quota < 5*1024*1024 || quota > 1024*1024*1024*1024 {
			return Config{}, fmt.Errorf("RESOURCE_QUOTA_BYTES must be between 5242880 and 1099511627776")
		}
		c.ResourceQuotaBytes = quota
	}
	c.SecureCookies = true
	if value := get("COOKIE_SECURE"); value != "" {
		if value != "true" && value != "false" {
			return Config{}, fmt.Errorf("COOKIE_SECURE must be true or false")
		}
		c.SecureCookies = value == "true"
	}
	if c.PublicOrigin != "" {
		origin, err := url.Parse(c.PublicOrigin)
		if err != nil || origin.User != nil || origin.Hostname() != c.WebAuthnRPID || net.ParseIP(c.WebAuthnRPID) != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "https" && !(origin.Scheme == "http" && c.WebAuthnRPID == "localhost")) {
			return Config{}, fmt.Errorf("PUBLIC_ORIGIN must be an exact HTTPS origin matching WEBAUTHN_RP_ID (HTTP localhost is allowed for development)")
		}
		if origin.Scheme == "https" && !c.SecureCookies {
			return Config{}, fmt.Errorf("COOKIE_SECURE must be true for an HTTPS PUBLIC_ORIGIN")
		}
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = "127.0.0.1:8080"
	}
	_, port, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		return Config{}, fmt.Errorf("HTTP_ADDR must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Config{}, fmt.Errorf("HTTP_ADDR must have a port between 1 and 65535")
	}
	u, err := url.Parse(c.DatabaseURL)
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
		return Config{}, fmt.Errorf("DATABASE_URL must be a PostgreSQL URL with host and database")
	}
	return c, nil
}
