package job

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

type Status string

const (
	Pending           Status = "pending"
	Running           Status = "running"
	Partial           Status = "partial"
	PartiallyReverted Status = "partially_reverted"
	Succeeded         Status = "succeeded"
	Reverted          Status = "reverted"
	Failed            Status = "failed"
)

// Operation tells the worker whether to run the queued plan or to compensate
// the batches it already committed.
type Operation string

const (
	Execute    Operation = "execute"
	Compensate Operation = "compensate"
)

var (
	ErrNotFound      = errors.New("job not found")
	ErrNoJob         = errors.New("no job available")
	ErrLeaseLost     = errors.New("job lease lost")
	ErrConflict      = errors.New("job conflicts with the stored execution")
	ErrNotResumable  = errors.New("only partial or partially reverted jobs can be resumed")
	ErrNotCancelable = errors.New("only a partial batch apply or forward can be cancelled")
	ErrIrreversible  = errors.New("the migration is irreversible; create a new migration to repair its data")
	ErrInvalidJob    = errors.New("invalid job")
)

// Terminal reports whether the queue will never hand the job out again.
func (s Status) Terminal() bool { return s == Succeeded || s == Reverted || s == Failed }

// Stopped reports whether the job stopped with committed batches that still
// need a resume or a cancellation.
func (s Status) Stopped() bool { return s == Partial || s == PartiallyReverted }

// Progress counts the batches and rows already committed by a batch migration
// out of the TotalRows fixed when its scope was determined.
type Progress struct {
	CommittedBatches int64 `json:"committed_batches"`
	CommittedRows    int64 `json:"committed_rows"`
	TotalRows        int64 `json:"total_rows"`
}

// Job is one queued execution. Request holds the reviewed snapshot that the
// worker executes, including the producer trace context.
type Job struct {
	ID          string                     `json:"id"`
	ExecutionID string                     `json:"execution_id"`
	DatabaseID  string                     `json:"database_id"`
	MigrationID string                     `json:"migration_id"`
	Name        string                     `json:"name"`
	ActorID     string                     `json:"actor_id"`
	Action      migration.Action           `json:"action"`
	Revision    int64                      `json:"revision"`
	Status      Status                     `json:"status"`
	Operation   Operation                  `json:"operation"`
	Attempts    int                        `json:"attempts"`
	Progress    Progress                   `json:"progress"`
	LastError   string                     `json:"last_error,omitempty"`
	AvailableAt time.Time                  `json:"available_at"`
	CreatedAt   time.Time                  `json:"created_at"`
	StartedAt   *time.Time                 `json:"started_at,omitempty"`
	FinishedAt  *time.Time                 `json:"finished_at,omitempty"`
	Request     migration.ExecutionRequest `json:"-"`
}

func (j *Job) describe() {
	j.ExecutionID = j.Request.ExecutionID
	j.DatabaseID = j.Request.Plan.DatabaseID
	j.MigrationID = j.Request.Plan.MigrationID
	j.Name = j.Request.Plan.Name
	j.ActorID = j.Request.ActorID
	j.Action = j.Request.Plan.Action
	j.Revision = j.Request.Plan.Revision
}

func validateRequest(request migration.ExecutionRequest) error {
	plan := request.Plan
	switch {
	case !validUUID(request.ExecutionID):
		return invalid("execution ID")
	case !validUUID(request.ApprovalID):
		return invalid("approval ID")
	case !validUUID(request.ActorID):
		return invalid("actor ID")
	case !validUUID(plan.DatabaseID):
		return invalid("database ID")
	case !validUUID(plan.MigrationID):
		return invalid("migration ID")
	case plan.Action != migration.Apply && plan.Action != migration.Rollback && plan.Action != migration.Forward:
		return invalid("action")
	case plan.Revision < 1:
		return invalid("revision")
	case strings.TrimSpace(plan.SQL) == "":
		return invalid("SQL")
	}
	return nil
}

// sameExecution ignores the trace context because a redelivered request may
// carry a newer producer span.
func sameExecution(stored, submitted migration.ExecutionRequest) bool {
	stored.TraceContext, submitted.TraceContext = telemetry.TraceContext{}, telemetry.TraceContext{}
	return stored == submitted
}

func invalid(field string) error { return fmt.Errorf("%w: %s", ErrInvalidJob, field) }

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16
}
