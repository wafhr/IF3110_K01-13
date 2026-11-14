//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

func TestMigrationWorkerIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	outages := &atomic.Int32{}
	runner := f.workerExecutor(t, outages)

	t.Run("apply, rollback, and forward run through the queue", func(t *testing.T) {
		definition := f.createMigration(t, "create worker audit", "CREATE TABLE worker_audit (id INT PRIMARY KEY)", "DROP TABLE worker_audit")
		for _, step := range []struct {
			action migration.Action
			tables int
		}{{migration.Apply, 1}, {migration.Rollback, 0}, {migration.Forward, 1}} {
			request := f.queueAction(t, definition, step.action)
			done := f.processJob(t, runner, request)
			if done.Status != job.Succeeded || done.Attempts != 1 || done.FinishedAt == nil {
				t.Fatalf("%s job = %+v", step.action, done)
			}
			assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename = 'worker_audit'`, step.tables)
			assertExecutionRecorded(t, f, request.ExecutionID, step.action)

			var detail struct {
				Job job.Job `json:"job"`
			}
			decode(t, f.reader.do(t, "GET", "/databases/"+f.databaseID+"/jobs/"+done.ID, nil, 200), &detail)
			if detail.Job.Status != job.Succeeded {
				t.Fatalf("job API reports %s for a finished job", detail.Job.Status)
			}
		}
	})

	t.Run("failed SQL fails the job without changing history", func(t *testing.T) {
		before := len(f.executionHistory(t, false).Executions)
		definition := f.createMigration(t, "broken statement", "CREATE TABLE worker_broken (", "DROP TABLE worker_broken")
		done := f.processJob(t, runner, f.queueAction(t, definition, migration.Apply))
		if done.Status != job.Failed || done.Attempts != 1 || !strings.HasPrefix(done.LastError, "SQLSTATE 42601") {
			t.Fatalf("failed job = %+v", done)
		}
		if after := len(f.executionHistory(t, false).Executions); after != before {
			t.Fatalf("history grew from %d to %d after a failed attempt", before, after)
		}
	})

	t.Run("transient target failure is retried", func(t *testing.T) {
		definition := f.createMigration(t, "create retry table", "CREATE TABLE worker_retry (id INT PRIMARY KEY)", "DROP TABLE worker_retry")
		outages.Store(1)
		done := f.processJob(t, runner, f.queueAction(t, definition, migration.Apply))
		if done.Status != job.Succeeded || done.Attempts != 2 {
			t.Fatalf("retried job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename = 'worker_retry'`, 1)
	})

	t.Run("duplicate delivery completes from commit evidence", func(t *testing.T) {
		definition := f.createMigration(t, "count deliveries", "CREATE TABLE worker_deliveries (id INT PRIMARY KEY); INSERT INTO worker_deliveries VALUES (1)", "DROP TABLE worker_deliveries")
		request := f.queueAction(t, definition, migration.Apply)
		if _, err := f.executeJob(request); err != nil {
			t.Fatal(err)
		}
		done := f.processJob(t, runner, request)
		if done.Status != job.Succeeded {
			t.Fatalf("duplicate job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM worker_deliveries`, 1)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_executions WHERE id = $1`, 1, request.ExecutionID)
	})

	t.Run("job of a crashed worker is recovered after its lease expires", func(t *testing.T) {
		definition := f.createMigration(t, "recover crashed job", "CREATE TABLE worker_recovered (id INT PRIMARY KEY)", "DROP TABLE worker_recovered")
		request := f.queueAction(t, definition, migration.Apply)
		crashed, err := f.jobs.Claim(context.Background(), "crashed-worker", 3*time.Second)
		if err != nil || crashed.ExecutionID != request.ExecutionID {
			t.Fatalf("crashed claim = %+v, %v", crashed, err)
		}
		execFixture(t, f.platform, `UPDATE migration_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, crashed.ID)
		done := f.processJob(t, runner, request)
		if done.Status != job.Succeeded || done.Attempts != 2 {
			t.Fatalf("recovered job = %+v", done)
		}
	})

	t.Run("revoked permission fails at revalidation", func(t *testing.T) {
		definition := f.createMigration(t, "create revoked table", "CREATE TABLE worker_revoked (id INT PRIMARY KEY)", "DROP TABLE worker_revoked")
		request := f.queueAction(t, definition, migration.Apply)
		if err := access.NewService(f.platform).SetDatabasePermission(context.Background(), f.editorGroupID, f.databaseID, access.ViewOnly); err != nil {
			t.Fatal(err)
		}
		done := f.processJob(t, runner, request)
		if done.Status != job.Failed || done.Attempts != 1 || done.LastError != access.ErrForbidden.Error() {
			t.Fatalf("revoked job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename = 'worker_revoked'`, 0)
	})
}

// workerExecutor opens targets through the registry and fails the next
// outages openings with a connection error to simulate an unreachable target.
func (f *migrationFixture) workerExecutor(t *testing.T, outages *atomic.Int32) *worker.Executor {
	t.Helper()
	open := func(ctx context.Context, id string) (*sql.DB, database.DBMS, error) {
		if outages.Add(-1) >= 0 {
			return nil, "", targetdb.ErrConnection
		}
		outages.Store(0)
		return f.openTarget(ctx, id)
	}
	executor, err := worker.NewExecutor(open, worker.MigrationPreparer(f.migrations))
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

// processJob runs a worker until the job of request is final or stopped.
func (f *migrationFixture) processJob(t *testing.T, runner worker.Runner, request migration.ExecutionRequest) job.Job {
	t.Helper()
	return f.processJobWithin(t, runner, request, 15*time.Second)
}

func (f *migrationFixture) processJobWithin(t *testing.T, runner worker.Runner, request migration.ExecutionRequest, limit time.Duration) job.Job {
	t.Helper()
	consumer, err := worker.New(f.jobs, runner, worker.Config{
		ID: "integration-worker", Concurrency: 2, PollInterval: 20 * time.Millisecond, Lease: 3 * time.Second,
		DrainTimeout: 2 * time.Second, Retry: worker.RetryPolicy{MaxAttempts: 3, BaseDelay: 50 * time.Millisecond, MaxDelay: 200 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- consumer.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Error(err)
		}
	}()
	queued := f.jobFor(t, request.ExecutionID)
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		current, err := f.jobs.Get(context.Background(), queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() || current.Status.Stopped() {
			return current
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", queued.ID)
	return job.Job{}
}

func (f *migrationFixture) jobFor(t *testing.T, executionID string) job.Job {
	t.Helper()
	var jobID string
	if err := f.platform.QueryRow(`SELECT id::text FROM migration_jobs WHERE execution_id = $1`, executionID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	queued, err := f.jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return queued
}

func assertExecutionRecorded(t *testing.T, f *migrationFixture, executionID string, action migration.Action) {
	t.Helper()
	for _, execution := range f.executionHistory(t, false).Executions {
		if execution.ID == executionID {
			if execution.Action != action || execution.Status != migration.Success {
				t.Fatalf("recorded execution = %+v", execution)
			}
			return
		}
	}
	t.Fatalf("execution %s missing from history", executionID)
}
