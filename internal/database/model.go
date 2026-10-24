// Package database manages the platform's registry of target databases.
package database

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidInput = errors.New("invalid database configuration")
	ErrNotFound     = errors.New("database not found")
	ErrConflict     = errors.New("database name already exists")
	ErrStorage      = errors.New("database registry storage failure")
)

// Database is public registry metadata. It never contains a password.
type Database struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	DatabaseName string    `json:"database_name"`
	Username     string    `json:"username"`
	SSLMode      string    `json:"ssl_mode"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ConnectionConfig is for backend use. Password is excluded from JSON responses.
type ConnectionConfig struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	DatabaseName string `json:"database_name"`
	Username     string `json:"username"`
	Password     string `json:"-"`
	SSLMode      string `json:"ssl_mode"`
}

func normalizeRegistration(name string, config ConnectionConfig) (string, ConnectionConfig, error) {
	name = strings.TrimSpace(name)
	if err := validateText("name", name, 100); err != nil {
		return "", ConnectionConfig{}, err
	}
	config, err := NormalizeConnectionConfig(config)
	return name, config, err
}

// NormalizeConnectionConfig validates settings for both saved registrations and
// connection checks performed before registration. It never modifies passwords.
func NormalizeConnectionConfig(config ConnectionConfig) (ConnectionConfig, error) {
	config.Host = strings.TrimSpace(config.Host)
	config.DatabaseName = strings.TrimSpace(config.DatabaseName)
	config.Username = strings.TrimSpace(config.Username)
	config.SSLMode = strings.ToLower(strings.TrimSpace(config.SSLMode))
	if config.Port == 0 {
		config.Port = 5432
	}
	if config.SSLMode == "" {
		config.SSLMode = "require"
	}
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"host", config.Host, 253},
		{"database_name", config.DatabaseName, 63},
		{"username", config.Username, 63},
	} {
		if err := validateText(field.name, field.value, field.max); err != nil {
			return ConnectionConfig{}, err
		}
	}
	if config.Port < 1 || config.Port > 65535 {
		return ConnectionConfig{}, fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalidInput)
	}
	if !utf8.ValidString(config.Password) || strings.ContainsRune(config.Password, '\x00') {
		return ConnectionConfig{}, fmt.Errorf("%w: password must be valid UTF-8 without NUL", ErrInvalidInput)
	}
	switch config.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return ConnectionConfig{}, fmt.Errorf("%w: unsupported ssl_mode", ErrInvalidInput)
	}
	return config, nil
}

func validateText(name, value string, max int) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') ||
		value == "" || utf8.RuneCountInString(value) > max {
		return fmt.Errorf("%w: %s must contain 1 to %d characters without NUL", ErrInvalidInput, name, max)
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
}
