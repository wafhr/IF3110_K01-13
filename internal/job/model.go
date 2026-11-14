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
	Pending   Status = "pending"
	Running   Status = "running"
	Partial   Status = "partial"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

var (
	ErrNotFound   = errors.New("job not found")
	ErrNoJob      = errors.New("no job available")
	ErrLeaseLost  = errors.New("job lease lost")
	ErrConflict   = errors.New("job conflicts with the stored execution")
	ErrInvalidJob = errors.New("invalid job")
)

// Terminal reports whether the queue will never hand the job out again.
func (s Status) Terminal() bool { return s == Succeeded || s == Failed }

type Progress struct {
	CommittedBatches int64 `json:"committed_batches"`
	CommittedRows    int64 `json:"committed_rows"`
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
