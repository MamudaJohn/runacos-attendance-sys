package config

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, addr, db string
		valid          bool
	}{
		{"defaults", "", "postgres://localhost/cabv", true},
		{"explicit", "127.0.0.1:9090", "postgresql://localhost/cabv", true},
		{"missing database", "", "", false},
		{"wrong driver", "", "sqlite://localhost/cabv", false},
		{"missing name", "", "postgres://localhost", false},
		{"bad address", "localhost", "postgres://localhost/cabv", false},
		{"bad port", "localhost:99999", "postgres://localhost/cabv", false},
		{"named port", "localhost:http", "postgres://localhost/cabv", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(func(k string) string {
				if k == "HTTP_ADDR" {
					return tc.addr
				}
				if k == "DATABASE_URL" {
					return tc.db
				}
				return ""
			})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if tc.valid && tc.addr == "" && c.HTTPAddr != "127.0.0.1:8080" {
				t.Fatal("incorrect default")
			}
		})
	}
}

func TestSecureCookies(t *testing.T) {
	for _, value := range []string{"", "true", "false", "invalid"} {
		c, err := Parse(func(key string) string {
			if key == "DATABASE_URL" {
				return "postgres://localhost/cabv"
			}
			if key == "COOKIE_SECURE" {
				return value
			}
			return ""
		})
		if value == "invalid" {
			if err == nil {
				t.Fatal("accepted invalid boolean")
			}
			continue
		}
		if err != nil || c.SecureCookies != (value != "false") {
			t.Fatalf("value %q config=%+v error=%v", value, c, err)
		}
	}
}

func TestResourceConfiguration(t *testing.T) {
	for _, tc := range []struct {
		quota string
		valid bool
	}{{"", true}, {"5242880", true}, {"104857600", true}, {"0", false}, {"-1", false}, {"invalid", false}, {"1099511627777", false}} {
		t.Run(tc.quota, func(t *testing.T) {
			c, err := Parse(func(key string) string {
				switch key {
				case "DATABASE_URL":
					return "postgres://localhost/cabv"
				case "RESOURCE_QUOTA_BYTES":
					return tc.quota
				}
				return ""
			})
			if (err == nil) != tc.valid {
				t.Fatalf("config=%+v err=%v", c, err)
			}
			if tc.valid && c.ResourceDir != ".local/resources" {
				t.Fatal("incorrect storage default")
			}
		})
	}
}
