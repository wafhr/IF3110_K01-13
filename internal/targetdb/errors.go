// Package targetdb manages reusable connections to registered target databases.
package targetdb

import (
	"context"
	"errors"
	"net"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

var (
	ErrConnection       = errors.New("target database connection failed")
	ErrAuthentication   = errors.New("target database authentication failed")
	ErrPermissionDenied = errors.New("target database access denied")
	ErrClosed           = errors.New("target database manager is closed")
)

// ConnectionError preserves safe error categories without exposing driver error
// messages, connection strings, passwords, or server details.
func ConnectionError(err error) error {
	if err == nil {
		return nil
	}
	for _, safe := range []error{
		context.Canceled, context.DeadlineExceeded,
		database.ErrNotFound, database.ErrInvalidInput, database.ErrStorage, database.ErrConflict,
		ErrClosed, ErrAuthentication, ErrPermissionDenied, ErrConnection,
	} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "28000", "28P01":
			return ErrAuthentication
		case "42501":
			return ErrPermissionDenied
		}
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return ErrConnection
}
