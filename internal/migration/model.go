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
	ErrExecutionUnavailable  = errors.New("migration execution dependencies unavailable")
	ErrApprovalRequired      = fmt.Errorf("%w: approval is missing or no longer valid", ErrConflict)
	ErrNotFound              = errors.New("migration not found")
	ErrConflict              = errors.New("migration conflict")
	ErrStorage               = errors.New("migration storage failure")
	ErrInvalidHistory        = fmt.Errorf("%w: execution history is inconsistent", ErrStorage)
	ErrImmutableMigration    = fmt.Errorf("%w: executed migration cannot be revised", ErrConflict)
	ErrInvalidMigration      = errors.New("invalid migration request")
	ErrIrreversibleMigration = fmt.Errorf("%w: migration is irreversible", ErrInvalidMigration)
)

// Status menyatakan hasil eksekusi. Workflow dan antrean memiliki status masing-masing.
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

// Migration menyimpan identitas definisi pada satu database target yang terdaftar.
// CurrentRevision menunjuk revisi definisi terbaru, terpisah dari posisi migration aktif.
type Migration struct {
	ID              string     `json:"id"`
	DatabaseID      string     `json:"database_id"`
	Name            string     `json:"name"`
	CurrentRevision int64      `json:"current_revision"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	FrozenAt        *time.Time `json:"frozen_at,omitempty"`
}

// Revision menyimpan SQL asli untuk ditinjau dan dieksekusi. Nomor revisi dimulai
// dari satu untuk setiap migration dan bersifat immutable sejak percobaan eksekusi.
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

// Execution mencatat satu percobaan eksekusi beserta salinan SQL yang dijalankan.
// RevertsExecutionID menghubungkan rollback dengan eksekusi committed yang dibatalkan.
// Migration history Milestone 2 menyimpan eksekusi yang berhasil di-commit.
type Execution struct {
	Sequence           int64      `json:"sequence"`
	Name               string     `json:"name"`
	LegacyVersion      *int64     `json:"legacy_version,omitempty"`
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

// Record mempertahankan format respons history Milestone 1 selama penyimpanan
// dan API beralih ke history eksekusi yang mencatat revisi migration.
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

// NormalizeRevision memvalidasi pasangan SQL dan merapikan alasan irreversible.
// Isi SQL dipertahankan persis untuk approval, eksekusi, dan salinan history.
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

// SQLFor memilih SQL asli sesuai tindakan. Pemanggil wajib memeriksa urutan
// linear, permission, approval, dan perlindungan revisi pada penyimpanan.
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
