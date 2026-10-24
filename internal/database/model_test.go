package database

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validConfig() ConnectionConfig {
	return ConnectionConfig{Host: "localhost", DatabaseName: "app", Username: "reader", Password: "secret-password"}
}

func TestNormalizeRegistration(t *testing.T) {
	config := validConfig()
	config.Host, config.DatabaseName, config.Username = " localhost ", " app ", " reader "
	config.Password = " password with spaces "
	name, got, err := normalizeRegistration(" Main database ", config)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Main database" || got.Host != "localhost" || got.DatabaseName != "app" || got.Username != "reader" {
		t.Fatalf("registration was not normalized: name=%q, config=%+v", name, got)
	}
	if got.Port != 5432 || got.SSLMode != "require" || got.Password != config.Password {
		t.Fatal("defaults or password whitespace were not preserved")
	}
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			config := validConfig()
			config.Port, config.SSLMode, config.Password = 5433, strings.ToUpper(mode), ""
			_, got, err := normalizeRegistration(strings.Repeat("界", 100), config)
			if err != nil || got.Port != 5433 || got.SSLMode != mode || got.Password != "" {
				t.Fatalf("valid explicit settings rejected: %v", err)
			}
		})
	}
}

func TestNormalizeRegistrationRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*string, *ConnectionConfig)
	}{
		{"empty name", func(name *string, _ *ConnectionConfig) { *name = "  " }},
		{"long name", func(name *string, _ *ConnectionConfig) { *name = strings.Repeat("a", 101) }},
		{"NUL name", func(name *string, _ *ConnectionConfig) { *name = "main\x00" }},
		{"invalid UTF-8 name", func(name *string, _ *ConnectionConfig) { *name = string([]byte{255}) }},
		{"empty host", func(_ *string, c *ConnectionConfig) { c.Host = " " }},
		{"long host", func(_ *string, c *ConnectionConfig) { c.Host = strings.Repeat("a", 254) }},
		{"NUL host", func(_ *string, c *ConnectionConfig) { c.Host = "local\x00host" }},
		{"negative port", func(_ *string, c *ConnectionConfig) { c.Port = -1 }},
		{"large port", func(_ *string, c *ConnectionConfig) { c.Port = 65536 }},
		{"empty database name", func(_ *string, c *ConnectionConfig) { c.DatabaseName = "" }},
		{"long database name", func(_ *string, c *ConnectionConfig) { c.DatabaseName = strings.Repeat("a", 64) }},
		{"empty username", func(_ *string, c *ConnectionConfig) { c.Username = "" }},
		{"long username", func(_ *string, c *ConnectionConfig) { c.Username = strings.Repeat("a", 64) }},
		{"NUL password", func(_ *string, c *ConnectionConfig) { c.Password = "private\x00value" }},
		{"invalid UTF-8 password", func(_ *string, c *ConnectionConfig) { c.Password = string([]byte{255}) }},
		{"invalid SSL mode", func(_ *string, c *ConnectionConfig) { c.SSLMode = "unknown-mode" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			name, config := "Main", validConfig()
			test.change(&name, &config)
			_, _, err := normalizeRegistration(name, config)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want invalid input", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("validation error disclosed password")
			}
		})
	}
}

func TestConnectionConfigJSONExcludesPassword(t *testing.T) {
	config := validConfig()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), config.Password) || strings.Contains(string(encoded), "password") {
		t.Fatalf("password appears in JSON: %s", encoded)
	}
}

func TestValidID(t *testing.T) {
	for _, test := range []struct {
		id   string
		want bool
	}{
		{"00000000-0000-4000-8000-000000000001", true},
		{"ABCDEF01-1234-4567-8901-123456789ABC", true},
		{"", false},
		{"not-a-uuid", false},
		{"000000000000-4000-8000-000000000001", false},
		{"00000000-0000-4000-8000-00000000000z", false},
	} {
		if got := validID(test.id); got != test.want {
			t.Errorf("validID(%q) = %t, want %t", test.id, got, test.want)
		}
	}
}
