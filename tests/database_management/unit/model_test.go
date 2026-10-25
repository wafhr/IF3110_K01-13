package database_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func validConfig() database.ConnectionConfig {
	return database.ConnectionConfig{Host: "localhost", DatabaseName: "app", Username: "reader", Password: "secret-password"}
}

func TestNormalizeConnectionConfig(t *testing.T) {
	config := validConfig()
	config.Host, config.DatabaseName, config.Username = " localhost ", " app ", " reader "
	config.Password = " password with spaces "
	got, err := database.NormalizeConnectionConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "localhost" || got.DatabaseName != "app" || got.Username != "reader" {
		t.Fatalf("connection config was not normalized: %+v", got)
	}
	if got.Port != 5432 || got.SSLMode != "require" || got.Password != config.Password {
		t.Fatal("defaults or password whitespace were not preserved")
	}
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		t.Run(mode, func(t *testing.T) {
			config := validConfig()
			config.Port, config.SSLMode, config.Password = 5433, strings.ToUpper(mode), ""
			got, err := database.NormalizeConnectionConfig(config)
			if err != nil || got.Port != 5433 || got.SSLMode != mode || got.Password != "" {
				t.Fatalf("valid explicit settings rejected: %v", err)
			}
		})
	}
}

func TestNormalizeMySQLConnectionConfig(t *testing.T) {
	config := validConfig()
	config.DBMS, config.Port, config.SSLMode = database.MySQL, 0, ""
	got, err := database.NormalizeConnectionConfig(config)
	if err != nil || got.DBMS != database.MySQL || got.Port != 3306 || got.SSLMode != "require" {
		t.Fatalf("MySQL defaults = %+v, %v", got, err)
	}
	config.DatabaseName = strings.Repeat("d", 64)
	config.Username = strings.Repeat("u", 32)
	if _, err := database.NormalizeConnectionConfig(config); err != nil {
		t.Fatalf("valid MySQL identifier limits rejected: %v", err)
	}
	config.Username += "u"
	if _, err := database.NormalizeConnectionConfig(config); !errors.Is(err, database.ErrInvalidInput) {
		t.Fatalf("oversized MySQL username error = %v", err)
	}
	config = validConfig()
	config.DBMS, config.SSLMode = database.MySQL, "verify-full"
	if _, err := database.NormalizeConnectionConfig(config); !errors.Is(err, database.ErrInvalidInput) {
		t.Fatalf("unsupported MySQL TLS mode error = %v", err)
	}
}

func TestNormalizeConnectionConfigRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*database.ConnectionConfig)
	}{
		{"empty host", func(c *database.ConnectionConfig) { c.Host = " " }},
		{"long host", func(c *database.ConnectionConfig) { c.Host = strings.Repeat("a", 254) }},
		{"NUL host", func(c *database.ConnectionConfig) { c.Host = "local\x00host" }},
		{"negative port", func(c *database.ConnectionConfig) { c.Port = -1 }},
		{"large port", func(c *database.ConnectionConfig) { c.Port = 65536 }},
		{"empty database name", func(c *database.ConnectionConfig) { c.DatabaseName = "" }},
		{"long database name", func(c *database.ConnectionConfig) { c.DatabaseName = strings.Repeat("a", 64) }},
		{"empty username", func(c *database.ConnectionConfig) { c.Username = "" }},
		{"long username", func(c *database.ConnectionConfig) { c.Username = strings.Repeat("a", 64) }},
		{"NUL password", func(c *database.ConnectionConfig) { c.Password = "private\x00value" }},
		{"invalid UTF-8 password", func(c *database.ConnectionConfig) { c.Password = string([]byte{255}) }},
		{"invalid SSL mode", func(c *database.ConnectionConfig) { c.SSLMode = "unknown-mode" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := validConfig()
			test.change(&config)
			_, err := database.NormalizeConnectionConfig(config)
			if !errors.Is(err, database.ErrInvalidInput) {
				t.Fatalf("error = %v, want invalid input", err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
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
