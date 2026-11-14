package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

var testPolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 5 * time.Second}

func postgresError(code string) error {
	return fmt.Errorf("execute migration: %w", &pgconn.PgError{Code: code, Message: "server says no", Detail: "Key (email)=(private@example.com) already exists."})
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want failureKind
	}{
		{name: "syntax error", err: postgresError("42601"), want: permanentFailure},
		{name: "unique violation", err: postgresError("23505"), want: permanentFailure},
		{name: "connection exception", err: postgresError("08006"), want: transientFailure},
		{name: "serialization failure", err: postgresError("40001"), want: transientFailure},
		{name: "deadlock", err: postgresError("40P01"), want: transientFailure},
		{name: "lock not available", err: postgresError("55P03"), want: transientFailure},
		{name: "admin shutdown", err: postgresError("57P01"), want: transientFailure},
		{name: "out of memory", err: postgresError("53200"), want: transientFailure},
		{name: "outdated plan", err: fmt.Errorf("prepare: %w", migration.ErrConflict), want: permanentFailure},
		{name: "approval withdrawn", err: migration.ErrApprovalRequired, want: permanentFailure},
		{name: "invalid history", err: migration.ErrInvalidHistory, want: permanentFailure},
		{name: "permission revoked", err: access.ErrForbidden, want: permanentFailure},
		{name: "registration removed", err: database.ErrNotFound, want: permanentFailure},
		{name: "unsupported target", err: ErrUnsupportedTarget, want: permanentFailure},
		{name: "target unreachable", err: targetdb.ErrConnection, want: transientFailure},
		{name: "storage failure", err: migration.ErrStorage, want: transientFailure},
		{name: "providers missing", err: migration.ErrExecutionUnavailable, want: transientFailure},
		{name: "bad connection", err: driver.ErrBadConn, want: transientFailure},
		{name: "unknown error", err: errors.New("surprise"), want: transientFailure},
		{name: "uncertain commit", err: fmt.Errorf("%w: %w", ErrCommitUncertain, postgresError("42601")), want: uncertainFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.err); got != test.want {
				t.Fatalf("classify = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		attempt   int
		wantRetry bool
		wantDelay time.Duration
	}{
		{name: "permanent failure never retries", err: postgresError("42601"), attempt: 1},
		{name: "first transient failure", err: targetdb.ErrConnection, attempt: 1, wantRetry: true, wantDelay: time.Second},
		{name: "second transient failure backs off", err: targetdb.ErrConnection, attempt: 2, wantRetry: true, wantDelay: 2 * time.Second},
		{name: "transient failure at the attempt limit", err: targetdb.ErrConnection, attempt: 3},
		{name: "uncertain commit ignores the attempt limit", err: ErrCommitUncertain, attempt: 9, wantRetry: true, wantDelay: 5 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := testPolicy.Decide(test.err, test.attempt)
			if decision.Retry != test.wantRetry || decision.Delay != test.wantDelay || decision.Reason == "" {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestBackoffIsCapped(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for attempt, delay := range want {
		if got := testPolicy.backoff(attempt + 1); got != delay {
			t.Errorf("attempt %d delay = %s, want %s", attempt+1, got, delay)
		}
	}
	if got := testPolicy.backoff(1 << 20); got != testPolicy.MaxDelay {
		t.Fatalf("large attempt delay = %s", got)
	}
}

func TestFailureReasonHidesDetails(t *testing.T) {
	tests := map[string]error{
		"SQLSTATE 23505: server says no":                             postgresError("23505"),
		"target database connection failed":                          fmt.Errorf("dial tcp 10.0.0.5:5432: %w", targetdb.ErrConnection),
		"migration conflict":                                         fmt.Errorf("plan changed: %w", migration.ErrConflict),
		"commit outcome is uncertain":                                fmt.Errorf("%w: write: broken pipe", ErrCommitUncertain),
		"migration attempt failed":                                   errors.New("password=secret host=db.internal"),
		"migration jobs support PostgreSQL targets only":             ErrUnsupportedTarget,
		"migration execution dependencies unavailable":               migration.ErrExecutionUnavailable,
		"migration conflict: approval is missing or no longer valid": migration.ErrApprovalRequired,
	}
	for want, err := range tests {
		got := failureReason(err)
		if got != want {
			t.Errorf("failureReason(%v) = %q, want %q", err, got, want)
		}
		if strings.Contains(got, "private@example.com") || strings.Contains(got, "secret") || strings.Contains(got, "10.0.0.5") {
			t.Errorf("failureReason leaked details: %q", got)
		}
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	for _, policy := range []RetryPolicy{
		{MaxAttempts: 0, BaseDelay: time.Second, MaxDelay: time.Second},
		{MaxAttempts: 1, BaseDelay: 0, MaxDelay: time.Second},
		{MaxAttempts: 1, BaseDelay: time.Minute, MaxDelay: time.Second},
	} {
		if policy.validate() == nil {
			t.Errorf("policy %+v accepted", policy)
		}
	}
	if err := DefaultRetryPolicy().validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStateUpdateKeepsContextCancellationSeparate(t *testing.T) {
	if classify(context.Canceled) != transientFailure {
		t.Fatal("a cancelled attempt must stay retryable")
	}
}
