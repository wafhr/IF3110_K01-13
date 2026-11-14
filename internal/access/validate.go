package access

import (
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

type inputError struct{ reason string }

func invalidInput(reason string) error { return &inputError{reason: reason} }

func (e *inputError) Error() string { return e.reason }

func (e *inputError) Unwrap() error { return ErrInvalidInput }

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 64 {
		return "", invalidInput("username must contain 3 to 64 characters")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' && r != '.' {
			return "", invalidInput("username may contain only letters, numbers, dot, dash, and underscore")
		}
	}
	return username, nil
}

func validatePassword(password string) error {
	if utf8.RuneCountInString(password) < 12 || len(password) > 1024 {
		return invalidInput("password must contain at least 12 characters and be at most 1024 bytes")
	}
	return nil
}

func normalizeGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return "", invalidInput("group name must contain 1 to 100 characters")
	}
	return name, nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16
}

func isUniqueViolation(err error) bool {
	return sqlState(err) == "23505"
}

func mapMissingOrError(err error) error {
	if sqlState(err) == "23503" {
		return ErrNotFound
	}
	return err
}

func sqlState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return state.SQLState()
	}
	return ""
}
