package access

import (
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

func normalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) < 3 || len(username) > 64 {
		return "", errors.New("username must contain 3 to 64 characters")
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' && r != '.' {
			return "", errors.New("username may contain only letters, numbers, dot, dash, and underscore")
		}
	}
	return username, nil
}

func validatePassword(password string) error {
	runes := utf8.RuneCountInString(password)
	if runes < 12 || len(password) > 1024 {
		return errors.New("password must contain at least 12 characters and be at most 1024 bytes")
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	compact := strings.ReplaceAll(value, "-", "")
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16
}

func isUniqueViolation(err error) bool {
	var state interface{ SQLState() string }
	return errors.As(err, &state) && state.SQLState() == "23505"
}

func mapMissingOrError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23503" {
		return ErrNotFound
	}
	return err
}
