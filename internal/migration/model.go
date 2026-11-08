package migration

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxMigrationNameBytes = 160
	MaxSQLBytes           = 1 << 20
)

var (
	ErrInvalidMigration      = errors.New("invalid migration request")
	ErrIrreversibleMigration = fmt.Errorf("%w: migration is irreversible", ErrInvalidMigration)
)

// Status describes an execution result. Workflow and queue states are separate.
type Status string

const (
	Running Status = "running"
	Success Status = "success"
	Failed  Status = "failed"
)

type Action string

const (
	Apply    Action = "apply"
	Rollback Action = "rollback"
	Forward  Action = "forward"
)

// Migration identifies a definition within one registered target database.
// CurrentRevision identifies its latest definition, independent of active history.
type Migration struct {
	ID              string    `json:"id"`
	DatabaseID      string    `json:"database_id"`
	Name            string    `json:"name"`
	CurrentRevision int64     `json:"current_revision"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Revision holds the exact SQL to review and execute. Revisions are numbered
// from one within a migration and become immutable once execution is attempted.
type Revision struct {
	MigrationID        string    `json:"migration_id"`
	Number             int64     `json:"number"`
	SQLUp              string    `json:"sql_up"`
	SQLDown            string    `json:"sql_down"`
	Irreversible       bool      `json:"irreversible"`
	IrreversibleReason string    `json:"irreversible_reason"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
}

// Execution is an individual attempt with a snapshot of the SQL actually run.
// RevertsExecutionID links a rollback to the committed execution it reverses.
// Only committed executions are stored in the Milestone 2 migration history.
type Execution struct {
	ID                 string     `json:"id"`
	DatabaseID         string     `json:"database_id"`
	MigrationID        string     `json:"migration_id"`
	Revision           int64      `json:"revision"`
	Action             Action     `json:"action"`
	SQL                string     `json:"sql"`
	Status             Status     `json:"status"`
	ExecutedBy         string     `json:"executed_by"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	RevertsExecutionID string     `json:"reverts_execution_id,omitempty"`
}

// Record preserves the Milestone 1 history response while its storage and API
// are migrated to revision-aware execution history.
type Record struct {
	Version    int64      `json:"version"`
	Name       string     `json:"name"`
	SQL        string     `json:"sql"`
	Status     Status     `json:"status"`
	ExecutedBy string     `json:"executed_by"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func NormalizeMigrationName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxMigrationNameBytes || !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("%w: name must contain 1 to %d bytes of valid UTF-8 without NUL", ErrInvalidMigration, MaxMigrationNameBytes)
	}
	return name, nil
}

// NormalizeRevision validates SQL pairing and normalizes the explanatory reason.
// SQL bytes are preserved for approval, execution, and history snapshots.
func NormalizeRevision(revision Revision) (Revision, error) {
	if revision.Number < 1 {
		return Revision{}, fmt.Errorf("%w: revision number must be positive", ErrInvalidMigration)
	}
	if err := validateSQL("sql_up", revision.SQLUp); err != nil {
		return Revision{}, err
	}
	revision.IrreversibleReason = strings.TrimSpace(revision.IrreversibleReason)
	if !utf8.ValidString(revision.IrreversibleReason) || strings.ContainsRune(revision.IrreversibleReason, '\x00') {
		return Revision{}, fmt.Errorf("%w: irreversible_reason must be valid UTF-8 without NUL", ErrInvalidMigration)
	}
	if revision.Irreversible {
		if revision.IrreversibleReason == "" || revision.SQLDown != "" {
			return Revision{}, fmt.Errorf("%w: irreversible migrations require a reason and an empty sql_down", ErrInvalidMigration)
		}
		return revision, nil
	}
	if revision.IrreversibleReason != "" {
		return Revision{}, fmt.Errorf("%w: irreversible_reason requires an irreversible migration", ErrInvalidMigration)
	}
	if err := validateSQL("sql_down", revision.SQLDown); err != nil {
		return Revision{}, err
	}
	return revision, nil
}

// SQLFor selects the original SQL for an action. Callers must check linear
// ordering, permissions, approvals, and persisted revision immutability.
func (revision Revision) SQLFor(action Action) (string, error) {
	revision, err := NormalizeRevision(revision)
	if err != nil {
		return "", err
	}
	switch action {
	case Apply, Forward:
		return revision.SQLUp, nil
	case Rollback:
		if revision.Irreversible {
			return "", ErrIrreversibleMigration
		}
		return revision.SQLDown, nil
	default:
		return "", fmt.Errorf("%w: unsupported migration action", ErrInvalidMigration)
	}
}

func validateSQL(field, statement string) error {
	if strings.TrimSpace(statement) == "" || len(statement) > MaxSQLBytes || !utf8.ValidString(statement) || strings.ContainsRune(statement, '\x00') {
		return fmt.Errorf("%w: %s must contain 1 to %d bytes of valid UTF-8 without NUL", ErrInvalidMigration, field, MaxSQLBytes)
	}
	return nil
}
