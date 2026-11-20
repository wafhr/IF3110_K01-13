package targetdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

type sqlStateError struct{ code string }

func (e sqlStateError) Error() string {
	return "server rejected login for user secret_user at 10.0.0.5"
}
func (e sqlStateError) SQLState() string { return e.code }

type timeoutError struct{}

var _ net.Error = timeoutError{}

func (timeoutError) Error() string   { return "dial tcp 10.0.0.5:5432: i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestConnectionErrorKeepsOnlySafeCategories(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "nil", err: nil, want: nil},
		{name: "canceled", err: fmt.Errorf("query: %w", context.Canceled), want: context.Canceled},
		{name: "known registry error", err: fmt.Errorf("load: %w", database.ErrNotFound), want: database.ErrNotFound},
		{name: "closed manager", err: ErrClosed, want: ErrClosed},
		{name: "invalid password", err: sqlStateError{code: "28P01"}, want: ErrAuthentication},
		{name: "invalid authorization", err: sqlStateError{code: "28000"}, want: ErrAuthentication},
		{name: "insufficient privilege", err: sqlStateError{code: "42501"}, want: ErrPermissionDenied},
		{name: "network timeout", err: timeoutError{}, want: context.DeadlineExceeded},
		{name: "unknown driver error", err: errors.New("pq: password=secret host=10.0.0.5"), want: ErrConnection},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ConnectionError(test.err)
			if test.want == nil {
				if got != nil {
					t.Fatalf("ConnectionError(nil) = %v", got)
				}
				return
			}
			if got != test.want {
				t.Fatalf("ConnectionError() = %v, want %v", got, test.want)
			}
			if strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "10.0.0.5") {
				t.Fatalf("ConnectionError leaked driver details: %q", got)
			}
		})
	}
}
