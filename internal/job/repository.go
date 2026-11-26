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
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

//go:embed schema.sql
var schema string

const jobColumns = `j.id::text, j.payload, j.status, j.operation, j.attempts, j.committed_batches, j.committed_rows,
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
	var waitSeconds float64
	job, err := scanJob(r.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT next.id, GREATEST(EXTRACT(EPOCH FROM (now() - CASE
                WHEN next.status = 'running' THEN next.lease_expires_at ELSE next.available_at END)), 0)::double precision AS wait_seconds
			FROM migration_jobs next
			WHERE ((next.status = 'pending' AND next.available_at <= now())
			    OR (next.status = 'running' AND next.lease_expires_at <= now()))
			  AND NOT EXISTS (
			      SELECT 1 FROM migration_jobs earlier
			      WHERE earlier.database_id = next.database_id
			        AND earlier.queue_position < next.queue_position
			        AND earlier.status IN ('pending', 'running', 'partial', 'partially_reverted'))
			ORDER BY next.queue_position
			LIMIT 1
			FOR UPDATE SKIP LOCKED),
		interrupted AS (
			INSERT INTO migration_attempts (`+attemptColumns+`)
			SELECT `+attemptValues("old", "'interrupted'", "'lease expired before the attempt finished'", "old.lease_expires_at")+`
			FROM migration_jobs old JOIN candidate ON candidate.id = old.id
			WHERE old.status = 'running')
		UPDATE migration_jobs j
		SET status = 'running', attempts = j.attempts + 1, current_attempt = j.attempts + 1,
		    attempt_started_at = now(), lease_owner = $1,
		    lease_expires_at = now() + $2 * interval '1 millisecond',
		    started_at = COALESCE(j.started_at, now()), updated_at = now()
		FROM candidate
		WHERE j.id = candidate.id
		RETURNING `+jobColumns+`, candidate.wait_seconds`, workerID, lease.Milliseconds()), &waitSeconds)
	job.QueueWaitSeconds = waitSeconds
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	return job, err
}

// CountByStatus membaca status persisten dari antrean bersama untuk setiap scrape.
func (r *Repository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	counts := map[string]int64{
		string(Pending): 0, string(Running): 0, string(Partial): 0, string(PartiallyReverted): 0,
		string(Succeeded): 0, string(Reverted): 0, string(Failed): 0,
	}
	rows, err := r.db.QueryContext(ctx, `SELECT status, count(*) FROM migration_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		if _, known := counts[status]; known {
			counts[status] = count
		}
	}
	return counts, rows.Err()
}

// Heartbeat extends the lease while the worker still owns the job.
func (r *Repository) Heartbeat(ctx context.Context, jobID, workerID string, lease time.Duration) error {
	if lease < time.Millisecond {
		return errors.New("heartbeat requires a lease of at least one millisecond")
	}
	return r.updateLeased(ctx, jobID, workerID, `lease_expires_at = now() + $3 * interval '1 millisecond'`, lease.Milliseconds())
}

// UpdateProgress records the progress reported after a commit by the lease
// holder and marks the job as having changed the target database.
func (r *Repository) UpdateProgress(ctx context.Context, jobID, workerID string, progress Progress) error {
	return r.updateLeased(ctx, jobID, workerID, `committed_batches = $3, committed_rows = $4, total_rows = $5, touched = TRUE`,
		progress.CommittedBatches, progress.CommittedRows, progress.TotalRows)
}

// Resume queues a stopped job again with a fresh attempt budget. The worker
// continues its operation from the checkpoint on the target database.
func (r *Repository) Resume(ctx context.Context, jobID string) error {
	return r.requeue(ctx, jobID, ``, `status IN ('partial', 'partially_reverted')`, ErrNotResumable)
}

// Cancel turns a partial apply or forward into a compensation that reverts
// its committed batches in reverse order.
func (r *Repository) Cancel(ctx context.Context, jobID string) error {
	return r.requeue(ctx, jobID, `operation = 'compensate', `,
		`status = 'partial' AND operation = 'execute' AND payload #>> '{plan,action}' <> 'rollback'`, ErrNotCancelable)
}

func (r *Repository) requeue(ctx context.Context, jobID, assignments, condition string, rejected error) error {
	if !uuid.Valid(jobID) {
		return ErrNotFound
	}
	result, err := r.db.ExecContext(ctx, `UPDATE migration_jobs SET `+assignments+`status = 'pending', attempts = 0,
		available_at = now(), updated_at = now() WHERE id = $1 AND `+condition, jobID)
	if err != nil {
		return fmt.Errorf("requeue job %s: %w", jobID, err)
	}
	requeued, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("requeue job %s: %w", jobID, err)
	}
	if requeued == 0 {
		if _, err := r.Get(ctx, jobID); err != nil {
			return err
		}
		return rejected
	}
	return nil
}

// Complete finishes the job: a compensation ends as reverted, anything else
// as succeeded.
func (r *Repository) Complete(ctx context.Context, jobID, workerID string) error {
	return r.finishAttempt(ctx, jobID, workerID, `status = CASE WHEN operation = 'compensate' THEN 'reverted' ELSE 'succeeded' END,
		last_error = '', `+finished, "updated.status", "''")
}

// Fail stops the job after a failure that will not be retried. committed tells
// whether the failed attempt itself committed a change. A job that never
// changed the target fails. One that committed batches stops as partial, or as
// partially reverted when it was reverting them, and keeps holding later jobs.
func (r *Repository) Fail(ctx context.Context, jobID, workerID, reason string, committed bool) error {
	return r.finishAttempt(ctx, jobID, workerID, `touched = touched OR $4,
		status = CASE
			WHEN NOT (touched OR $4) THEN 'failed'
			WHEN operation = 'compensate' OR payload #>> '{plan,action}' = 'rollback' THEN 'partially_reverted'
			ELSE 'partial' END,
		finished_at = CASE WHEN touched OR $4 THEN NULL ELSE now() END,
		last_error = $3, `+unleased, "updated.status", "updated.last_error", reason, committed)
}

// Retry returns the job to the queue after delay. The attempt count is kept so
// the worker can bound the number of retries.
func (r *Repository) Retry(ctx context.Context, jobID, workerID, reason string, delay time.Duration) error {
	if delay < 0 {
		return errors.New("retry delay must not be negative")
	}
	return r.finishAttempt(ctx, jobID, workerID, `status = 'pending', last_error = $3,
		available_at = now() + $4 * interval '1 millisecond', `+unleased, "'retrying'", "updated.last_error", reason, delay.Milliseconds())
}

// Release returns a job whose attempt was interrupted by a stopping worker. The
// attempt did not fail, so it is not counted against the retry budget.
func (r *Repository) Release(ctx context.Context, jobID, workerID string) error {
	return r.finishAttempt(ctx, jobID, workerID, `status = 'pending', attempts = j.attempts - 1, available_at = now(), `+unleased,
		"'released'", "'worker stopped before the attempt finished'")
}

func (r *Repository) Get(ctx context.Context, jobID string) (Job, error) {
	if !uuid.Valid(jobID) {
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
var listColumns = strings.Replace(jobColumns, "j.payload", `j.payload #- '{plan,sql}' #- '{plan,compensation_sql}'`, 1)

// ListByDatabase returns the most recent jobs of a database registration first.
func (r *Repository) ListByDatabase(ctx context.Context, databaseID string, limit int) ([]Job, error) {
	if !uuid.Valid(databaseID) {
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
	return r.updateLeasedStatement(ctx, jobID, `UPDATE migration_jobs j SET `+assignments+`, updated_at = now() WHERE `+leaseHolder,
		append([]any{jobID, workerID}, args...)...)
}

func (r *Repository) updateLeasedStatement(ctx context.Context, jobID, statement string, args ...any) error {
	if !uuid.Valid(jobID) {
		return ErrNotFound
	}
	result, err := r.db.ExecContext(ctx, statement, args...)
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

// finishAttempt applies the transition that ends an attempt and records the
// attempt in the same statement, so only the lease holder can write it.
func (r *Repository) finishAttempt(ctx context.Context, jobID, workerID, assignments, status, reason string, args ...any) error {
	return r.updateLeasedStatement(ctx, jobID, `WITH updated AS (UPDATE migration_jobs j SET `+assignments+`, updated_at = now() WHERE `+leaseHolder+` RETURNING j.*)
		INSERT INTO migration_attempts (`+attemptColumns+`) SELECT `+attemptValues("updated", status, reason, "now()")+` FROM updated`,
		append([]any{jobID, workerID}, args...)...)
}

const attemptColumns = `job_id, execution_id, database_id, migration_id, name, revision, action, operation, attempt, actor_id,
	sql, status, error, committed_batches, committed_rows, started_at, finished_at`

// attemptValues copies the identity and the exact SQL of an attempt from the
// job row source.
func attemptValues(source, status, reason, finishedAt string) string {
	return fmt.Sprintf(`%[1]s.id, %[1]s.execution_id, %[1]s.database_id, (%[1]s.payload #>> '{plan,migration_id}')::uuid, %[1]s.payload #>> '{plan,name}',
		(%[1]s.payload #>> '{plan,revision}')::bigint, %[1]s.payload #>> '{plan,action}', %[1]s.operation, %[1]s.current_attempt,
		(%[1]s.payload ->> 'actor_id')::uuid,
		COALESCE(CASE WHEN %[1]s.operation = 'compensate' THEN %[1]s.payload #>> '{plan,compensation_sql}' ELSE %[1]s.payload #>> '{plan,sql}' END, ''),
		%[2]s, %[3]s, %[1]s.committed_batches, %[1]s.committed_rows, COALESCE(%[1]s.attempt_started_at, %[1]s.started_at, now()), %[4]s`,
		source, status, reason, finishedAt)
}

const attemptListColumns = `a.id::text, a.sequence, a.job_id::text, a.execution_id::text, a.migration_id::text, a.name, a.revision, a.action,
	a.operation, a.attempt, a.actor_id::text, a.sql, a.status, a.error, a.committed_batches, a.committed_rows, a.started_at, a.finished_at`

// ListAttempts mengambil attempt sebelum nomor urut yang diberikan. Nilai nol
// memulai halaman pertama dari attempt terbaru.
func (r *Repository) ListAttempts(ctx context.Context, databaseID string, limit int, before int64) (AttemptPage, error) {
	if !uuid.Valid(databaseID) {
		return AttemptPage{}, ErrNotFound
	}
	if limit < 1 || limit > MaxListLimit {
		return AttemptPage{}, invalid("limit")
	}
	if before < 0 {
		return AttemptPage{}, invalid("before")
	}
	query := `SELECT ` + attemptListColumns + ` FROM migration_attempts a WHERE a.database_id = $1`
	args := []any{databaseID, limit + 1}
	if before != 0 {
		query += ` AND a.sequence < $3`
		args = append(args, before)
	}
	rows, err := r.db.QueryContext(ctx, query+` ORDER BY a.sequence DESC LIMIT $2`, args...)
	if err != nil {
		return AttemptPage{}, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()
	page := AttemptPage{Attempts: make([]Attempt, 0)}
	for rows.Next() {
		if len(page.Attempts) == limit {
			page.NextBefore = page.Attempts[limit-1].Sequence
			break
		}
		var a Attempt
		if err := rows.Scan(&a.ID, &a.Sequence, &a.JobID, &a.ExecutionID, &a.MigrationID, &a.Name, &a.Revision, &a.Action, &a.Operation,
			&a.Attempt, &a.ActorID, &a.SQL, &a.Status, &a.Error, &a.CommittedBatches, &a.CommittedRows, &a.StartedAt, &a.FinishedAt); err != nil {
			return AttemptPage{}, fmt.Errorf("read attempt: %w", err)
		}
		page.Attempts = append(page.Attempts, a)
	}
	return page, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner, extra ...any) (Job, error) {
	var job Job
	var payload []byte
	var startedAt, finishedAt sql.NullTime
	dest := []any{&job.ID, &payload, &job.Status, &job.Operation, &job.Attempts, &job.Progress.CommittedBatches,
		&job.Progress.CommittedRows, &job.Progress.TotalRows, &job.LastError, &job.AvailableAt, &job.CreatedAt,
		&startedAt, &finishedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
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
