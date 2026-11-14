package job

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

//go:embed schema.sql
var schema string

const jobColumns = `j.id::text, j.payload, j.status, j.attempts, j.committed_batches, j.committed_rows,
	j.total_rows, j.last_error, j.available_at, j.created_at, j.started_at, j.finished_at`

const MaxListLimit = 100

const leaseHolder = `j.id = $1 AND j.status = 'running' AND j.lease_owner = $2`

// Repository stores the migration queue in the platform database so jobs,
// attempts, and progress survive server and worker restarts.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("job repository requires a platform database")
	}
	return &Repository{db: db}, nil
}

func (r *Repository) Initialize(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job schema initialization: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(311005)`); err != nil {
		return fmt.Errorf("lock job schema initialization: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize job schema: %w", err)
	}
	return tx.Commit()
}

// Enqueue stores the complete request before returning the job. Submitting the
// same execution again returns the stored job instead of queueing a duplicate.
func (r *Repository) Enqueue(ctx context.Context, request migration.ExecutionRequest) (Job, error) {
	if err := validateRequest(request); err != nil {
		return Job{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return Job{}, fmt.Errorf("encode job payload: %w", err)
	}
	job, err := scanJob(r.db.QueryRowContext(ctx, `
		INSERT INTO migration_jobs AS j (execution_id, database_id, payload)
		VALUES ($1, $2, $3)
		ON CONFLICT (execution_id) DO NOTHING
		RETURNING `+jobColumns, request.ExecutionID, request.Plan.DatabaseID, payload))
	if !errors.Is(err, sql.ErrNoRows) {
		return job, err
	}
	stored, err := r.byExecution(ctx, request.ExecutionID)
	if err != nil {
		return Job{}, err
	}
	if !sameExecution(stored.Request, request) {
		return Job{}, ErrConflict
	}
	return stored, nil
}

// Claim leases the oldest runnable job whose database has no earlier unfinished
// job, so each database runs one migration at a time in submission order while
// different databases proceed in parallel. A running job whose lease expired
// is claimed again after its worker stopped.
func (r *Repository) Claim(ctx context.Context, workerID string, lease time.Duration) (Job, error) {
	if workerID == "" || lease < time.Millisecond {
		return Job{}, errors.New("claim requires a worker ID and a lease of at least one millisecond")
	}
	job, err := scanJob(r.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT next.id
			FROM migration_jobs next
			WHERE ((next.status = 'pending' AND next.available_at <= now())
			    OR (next.status = 'running' AND next.lease_expires_at <= now()))
			  AND NOT EXISTS (
			      SELECT 1 FROM migration_jobs earlier
			      WHERE earlier.database_id = next.database_id
			        AND earlier.queue_position < next.queue_position
			        AND earlier.status IN ('pending', 'running', 'partial'))
			ORDER BY next.queue_position
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		UPDATE migration_jobs j
		SET status = 'running', attempts = j.attempts + 1, lease_owner = $1,
		    lease_expires_at = now() + $2 * interval '1 millisecond',
		    started_at = COALESCE(j.started_at, now()), updated_at = now()
		FROM candidate
		WHERE j.id = candidate.id
		RETURNING `+jobColumns, workerID, lease.Milliseconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	return job, err
}

// Heartbeat extends the lease while the worker still owns the job.
func (r *Repository) Heartbeat(ctx context.Context, jobID, workerID string, lease time.Duration) error {
	if lease < time.Millisecond {
		return errors.New("heartbeat requires a lease of at least one millisecond")
	}
	return r.updateLeased(ctx, jobID, workerID, `lease_expires_at = now() + $3 * interval '1 millisecond'`, lease.Milliseconds())
}

// UpdateProgress records the committed progress reported by the lease holder.
func (r *Repository) UpdateProgress(ctx context.Context, jobID, workerID string, progress Progress) error {
	return r.updateLeased(ctx, jobID, workerID, `committed_batches = $3, committed_rows = $4, total_rows = $5`,
		progress.CommittedBatches, progress.CommittedRows, progress.TotalRows)
}

// MarkPartial stops a batch migration that committed some batches. The job
// holds later jobs of its database until it is resumed.
func (r *Repository) MarkPartial(ctx context.Context, jobID, workerID, reason string) error {
	return r.updateLeased(ctx, jobID, workerID, `status = 'partial', last_error = $3, `+unleased, reason)
}

// Resume queues a partial job again with a fresh attempt budget. The worker
// continues from the checkpoint on the target database.
func (r *Repository) Resume(ctx context.Context, jobID string) error {
	if !validUUID(jobID) {
		return ErrNotFound
	}
	result, err := r.db.ExecContext(ctx, `UPDATE migration_jobs SET status = 'pending', attempts = 0, available_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'partial'`, jobID)
	if err != nil {
		return fmt.Errorf("resume job %s: %w", jobID, err)
	}
	resumed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resume job %s: %w", jobID, err)
	}
	if resumed == 0 {
		if _, err := r.Get(ctx, jobID); err != nil {
			return err
		}
		return ErrNotPartial
	}
	return nil
}

func (r *Repository) Complete(ctx context.Context, jobID, workerID string) error {
	return r.updateLeased(ctx, jobID, workerID, `status = 'succeeded', last_error = '', `+finished)
}

func (r *Repository) Fail(ctx context.Context, jobID, workerID, reason string) error {
	return r.updateLeased(ctx, jobID, workerID, `status = 'failed', last_error = $3, `+finished, reason)
}

// Retry returns the job to the queue after delay. The attempt count is kept so
// the worker can bound the number of retries.
func (r *Repository) Retry(ctx context.Context, jobID, workerID, reason string, delay time.Duration) error {
	if delay < 0 {
		return errors.New("retry delay must not be negative")
	}
	return r.updateLeased(ctx, jobID, workerID, `status = 'pending', last_error = $3,
		available_at = now() + $4 * interval '1 millisecond', `+unleased, reason, delay.Milliseconds())
}

// Release returns a job whose attempt was interrupted by a stopping worker. The
// attempt did not fail, so it is not counted against the retry budget.
func (r *Repository) Release(ctx context.Context, jobID, workerID string) error {
	return r.updateLeased(ctx, jobID, workerID, `status = 'pending', attempts = j.attempts - 1, available_at = now(), `+unleased)
}

func (r *Repository) Get(ctx context.Context, jobID string) (Job, error) {
	if !validUUID(jobID) {
		return Job{}, ErrNotFound
	}
	job, err := scanJob(r.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs j WHERE j.id = $1`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// listColumns leaves the SQL out of the payload: listings never return it and
// it can reach a megabyte per job.
var listColumns = strings.Replace(jobColumns, "j.payload", `j.payload #- '{plan,sql}'`, 1)

// ListByDatabase returns the most recent jobs of a database registration first.
func (r *Repository) ListByDatabase(ctx context.Context, databaseID string, limit int) ([]Job, error) {
	if !validUUID(databaseID) {
		return nil, ErrNotFound
	}
	if limit < 1 || limit > MaxListLimit {
		return nil, invalid("limit")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+listColumns+` FROM migration_jobs j WHERE j.database_id = $1 ORDER BY j.queue_position DESC LIMIT $2`, databaseID, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	jobs := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *Repository) byExecution(ctx context.Context, executionID string) (Job, error) {
	job, err := scanJob(r.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs j WHERE j.execution_id = $1`, executionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

const (
	unleased = `lease_owner = NULL, lease_expires_at = NULL`
	finished = unleased + `, finished_at = now()`
)

// updateLeased applies a change only while workerID still holds the lease, so
// a worker that lost its lease cannot overwrite the job of its successor.
func (r *Repository) updateLeased(ctx context.Context, jobID, workerID, assignments string, args ...any) error {
	if !validUUID(jobID) {
		return ErrNotFound
	}
	result, err := r.db.ExecContext(ctx, `UPDATE migration_jobs j SET `+assignments+`, updated_at = now() WHERE `+leaseHolder,
		append([]any{jobID, workerID}, args...)...)
	if err != nil {
		return fmt.Errorf("update job %s: %w", jobID, err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update job %s: %w", jobID, err)
	}
	if updated == 0 {
		return ErrLeaseLost
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var payload []byte
	var startedAt, finishedAt sql.NullTime
	err := row.Scan(&job.ID, &payload, &job.Status, &job.Attempts, &job.Progress.CommittedBatches,
		&job.Progress.CommittedRows, &job.Progress.TotalRows, &job.LastError, &job.AvailableAt, &job.CreatedAt,
		&startedAt, &finishedAt)
	if err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal(payload, &job.Request); err != nil {
		return Job{}, fmt.Errorf("decode job %s payload: %w", job.ID, err)
	}
	job.StartedAt, job.FinishedAt = timeOrNil(startedAt), timeOrNil(finishedAt)
	job.describe()
	return job, nil
}

func timeOrNil(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
