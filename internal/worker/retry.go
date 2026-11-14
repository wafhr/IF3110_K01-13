package worker

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

var (
	ErrCommitUncertain   = errors.New("commit outcome is uncertain")
	ErrUnsupportedTarget = errors.New("migration jobs support PostgreSQL targets only")
)

type failureKind int

const (
	transientFailure failureKind = iota
	permanentFailure
	uncertainFailure
)

// RetryPolicy bounds retries of transient failures with exponential backoff.
// An uncertain commit is retried until the next attempt finds commit evidence
// or confirms that nothing was committed, so its job keeps later migrations of
// the same database waiting.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

type Decision struct {
	Retry  bool
	Delay  time.Duration
	Reason string
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: time.Minute}
}

func (p RetryPolicy) validate() error {
	if p.MaxAttempts < 1 || p.BaseDelay <= 0 || p.MaxDelay < p.BaseDelay {
		return errors.New("retry policy requires at least one attempt and a base delay not above the maximum delay")
	}
	return nil
}

// Decide chooses what happens to a job whose attempt number attempt failed.
func (p RetryPolicy) Decide(err error, attempt int) Decision {
	reason := failureReason(err)
	switch classify(err) {
	case permanentFailure:
		return Decision{Reason: reason}
	case uncertainFailure:
		return Decision{Retry: true, Delay: p.backoff(attempt), Reason: reason}
	}
	if attempt >= p.MaxAttempts {
		return Decision{Reason: reason}
	}
	return Decision{Retry: true, Delay: p.backoff(attempt), Reason: reason}
}

func (p RetryPolicy) backoff(attempt int) time.Duration {
	delay := p.BaseDelay
	for range max(attempt-1, 0) {
		if delay >= p.MaxDelay/2 {
			return p.MaxDelay
		}
		delay *= 2
	}
	return delay
}

var permanentErrors = []error{
	ErrUnsupportedTarget,
	migration.ErrApprovalRequired,
	migration.ErrImmutableMigration,
	migration.ErrIrreversibleMigration,
	migration.ErrInvalidHistory,
	migration.ErrInvalidMigration,
	migration.ErrConflict,
	migration.ErrNotFound,
	access.ErrForbidden,
	access.ErrNotFound,
	database.ErrNotFound,
	database.ErrInvalidInput,
	targetdb.ErrAuthentication,
	targetdb.ErrPermissionDenied,
}

var transientErrors = []error{
	targetdb.ErrConnection,
	targetdb.ErrClosed,
	migration.ErrExecutionUnavailable,
	migration.ErrStorage,
	context.DeadlineExceeded,
	driver.ErrBadConn,
	io.ErrUnexpectedEOF,
}

func classify(err error) failureKind {
	if errors.Is(err, ErrCommitUncertain) {
		return uncertainFailure
	}
	if matchesAny(err, permanentErrors) {
		return permanentFailure
	}
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		if transientState(postgres.Code) {
			return transientFailure
		}
		return permanentFailure
	}
	return transientFailure
}

// transientState covers connection loss, serialization and deadlock aborts,
// lock timeouts, server shutdown, and exhausted server resources.
func transientState(code string) bool {
	switch {
	case strings.HasPrefix(code, "08"), strings.HasPrefix(code, "53"), strings.HasPrefix(code, "58"):
		return true
	}
	switch code {
	case "40001", "40P01", "55P03", "57P01", "57P02", "57P03":
		return true
	}
	return false
}

// failureReason is stored on the job and shown to database viewers, so it
// keeps the SQLSTATE and primary message but never the error detail.
func failureReason(err error) string {
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		return "SQLSTATE " + postgres.Code + ": " + postgres.Message
	}
	for _, known := range append(append([]error{ErrCommitUncertain}, permanentErrors...), transientErrors...) {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	var network net.Error
	if errors.As(err, &network) {
		return targetdb.ErrConnection.Error()
	}
	return "migration attempt failed"
}

func matchesAny(err error, targets []error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
