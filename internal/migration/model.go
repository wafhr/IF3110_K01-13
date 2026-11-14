package migration

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const (
	MaxMigrationNameBytes = 160
	MaxSQLBytes           = 1 << 20
	MaxBatchSize          = 100000
	maxIdentifierBytes    = 63
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
	Batch              BatchSpec `json:"batch,omitzero"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
}

// BatchSpec runs a migration over key ranges of Table ordered by KeyColumn,
// committing each range separately. The SQL receives the first and last key of
// a batch as $1 and $2. Table is either "table" or "schema.table".
type BatchSpec struct {
	Table     string `json:"table"`
	KeyColumn string `json:"key_column"`
	Size      int    `json:"size"`
}

func (b BatchSpec) Enabled() bool { return b != BatchSpec{} }

// TableIdentifier splits Table into its optional schema and its name.
func (b BatchSpec) TableIdentifier() []string { return strings.Split(b.Table, ".") }

func (b BatchSpec) normalized(sqlUp, sqlDown string) (BatchSpec, error) {
	b.Table, b.KeyColumn = strings.TrimSpace(b.Table), strings.TrimSpace(b.KeyColumn)
	if !b.Enabled() {
		return b, nil
	}
	parts := b.TableIdentifier()
	if len(parts) > 2 || !validIdentifiers(append(parts, b.KeyColumn)...) {
		return BatchSpec{}, fmt.Errorf("%w: batch table and key_column must be identifiers of 1 to %d bytes", ErrInvalidMigration, maxIdentifierBytes)
	}
	if b.Size < 1 || b.Size > MaxBatchSize {
		return BatchSpec{}, fmt.Errorf("%w: batch size must be between 1 and %d", ErrInvalidMigration, MaxBatchSize)
	}
	for _, statement := range []string{sqlUp, sqlDown} {
		if statement != "" && (!strings.Contains(statement, "$1") || !strings.Contains(statement, "$2")) {
			return BatchSpec{}, fmt.Errorf("%w: batch SQL must use $1 and $2 as the first and last key of each batch", ErrInvalidMigration)
		}
	}
	return b, nil
}

func validIdentifiers(identifiers ...string) bool {
	for _, identifier := range identifiers {
		if identifier == "" || len(identifier) > maxIdentifierBytes || !utf8.ValidString(identifier) || strings.ContainsRune(identifier, '\x00') {
			return false
		}
	}
	return true
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

// ExecutionHistory memisahkan commit sukses dari attempt lama yang belum sukses.
type ExecutionHistory struct {
	Executions     []Execution `json:"executions"`
	LegacyAttempts []Record    `json:"legacy_attempts"`
}

// ActionPlan mengikat tindakan pada revisi asli dan posisi history saat rencana
// disusun. Executor memvalidasi ulang rencana setelah memperoleh lock database.
type ActionPlan struct {
	DatabaseID         string    `json:"database_id"`
	MigrationID        string    `json:"migration_id"`
	Revision           int64     `json:"revision"`
	Name               string    `json:"name"`
	Action             Action    `json:"action"`
	SQL                string    `json:"sql"`
	RevertsExecutionID string    `json:"reverts_execution_id,omitempty"`
	BaseExecutionID    string    `json:"base_execution_id,omitempty"`
	Batch              BatchSpec `json:"batch,omitzero"`
	CompensationSQL    string    `json:"compensation_sql,omitempty"`
}

// ExecutionRequest mengikat job pada aktor, approval, revisi, SQL, dan posisi
// history yang ditinjau. Antrean menyimpan seluruh payload secara persisten.
type ExecutionRequest struct {
	ExecutionID  string                 `json:"execution_id"`
	ApprovalID   string                 `json:"approval_id"`
	ActorID      string                 `json:"actor_id"`
	Plan         ActionPlan             `json:"plan"`
	TraceContext telemetry.TraceContext `json:"trace_context,omitzero"`
}

// ExecutionContext menambahkan identitas dari snapshot persisten dan memulihkan
// parent payload ketika context belum memiliki span aktif. Worker memulai span
// consumer sebelum membuka koneksi agar lock dan transaksi ikut berkorelasi.
func (r ExecutionRequest) ExecutionContext(ctx context.Context) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = telemetry.RestoreTraceContext(ctx, r.TraceContext)
	}
	return telemetry.WithLogAttrs(ctx,
		slog.String("actor", r.ActorID), slog.String("database_id", r.Plan.DatabaseID),
		slog.String("migration_id", r.Plan.MigrationID), slog.String("execution_id", r.ExecutionID),
		slog.Int64("revision", r.Plan.Revision), slog.String("action", string(r.Plan.Action)),
	)
}

type QueuedAction struct {
	JobID       string `json:"job_id"`
	ExecutionID string `json:"execution_id"`
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

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16
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
	} else {
		if revision.IrreversibleReason != "" {
			return Revision{}, fmt.Errorf("%w: irreversible_reason requires an irreversible migration", ErrInvalidMigration)
		}
		if err := validateSQL("sql_down", revision.SQLDown); err != nil {
			return Revision{}, err
		}
	}
	batch, err := revision.Batch.normalized(revision.SQLUp, revision.SQLDown)
	if err != nil {
		return Revision{}, err
	}
	revision.Batch = batch
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
