//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

func TestMigrationRecoveryIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	runner := f.workerExecutor(t, &atomic.Int32{})

	t.Run("a worker that outlived its lease is fenced by the database lock", func(t *testing.T) {
		definition := f.createMigration(t, "slow audit table",
			"SELECT pg_sleep(1.5); CREATE TABLE recovery_events (id INT PRIMARY KEY); INSERT INTO recovery_events VALUES (1)",
			"DROP TABLE recovery_events")
		request := f.queueAction(t, definition, migration.Apply)
		stale, err := f.jobs.Claim(context.Background(), "stale-worker", 3*time.Second)
		if err != nil || stale.ExecutionID != request.ExecutionID {
			t.Fatalf("stale claim = %+v, %v", stale, err)
		}
		staleResult := make(chan error, 1)
		go func() {
			_, err := runner.Run(context.Background(), request, nil)
			staleResult <- err
		}()
		f.waitForMigrationLock(t)
		execFixture(t, f.platform, `UPDATE migration_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, stale.ID)

		done := f.processJob(t, runner, request)
		if err := <-staleResult; err != nil {
			t.Fatalf("stale worker attempt: %v", err)
		}
		if done.Status != job.Succeeded || done.Attempts != 2 {
			t.Fatalf("recovered job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM recovery_events`, 1)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_executions WHERE id = $1`, 1, request.ExecutionID)
		if err := f.jobs.Complete(context.Background(), stale.ID, "stale-worker"); !errors.Is(err, job.ErrLeaseLost) {
			t.Fatalf("stale worker completed a job it no longer owns: %v", err)
		}
	})

	t.Run("a stopping worker releases its job without spending an attempt", func(t *testing.T) {
		definition := f.createMigration(t, "slow release table",
			"SELECT pg_sleep(3); CREATE TABLE recovery_released (id INT PRIMARY KEY)", "DROP TABLE recovery_released")
		request := f.queueAction(t, definition, migration.Apply)
		consumer, err := worker.New(f.jobs, runner, worker.Config{
			ID: "stopping-worker", Concurrency: 1, PollInterval: 20 * time.Millisecond, Lease: 3 * time.Second,
			DrainTimeout: 100 * time.Millisecond, Retry: worker.DefaultRetryPolicy(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		stopped := make(chan error, 1)
		go func() { stopped <- consumer.Run(ctx) }()
		queued := f.jobFor(t, request.ExecutionID)
		f.waitForJobStatus(t, queued.ID, job.Running)
		stop()
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		released, err := f.jobs.Get(context.Background(), queued.ID)
		if err != nil || released.Status != job.Pending || released.Attempts != 0 {
			t.Fatalf("released job = %+v, %v", released, err)
		}
		assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename = 'recovery_released'`, 0)

		done := f.processJob(t, runner, request)
		if done.Status != job.Succeeded || done.Attempts != 1 {
			t.Fatalf("job after restart = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename = 'recovery_released'`, 1)
	})

	t.Run("an interrupted compensation continues with the batches still applied", func(t *testing.T) {
		f.seedBatchAccounts(t)
		execFixture(t, f.target, `UPDATE batch_accounts SET note = 'poison' WHERE id = 4500`)
		definition := f.createBatchMigration(t, "raise limits",
			"UPDATE batch_accounts SET balance_cents = balance_cents + 7 / (CASE WHEN note = 'poison' THEN 0 ELSE 1 END) WHERE id BETWEEN $1 AND $2",
			"UPDATE batch_accounts SET balance_cents = balance_cents - 7 WHERE id BETWEEN $1 AND $2")
		request := f.queueAction(t, definition, migration.Apply)
		partial := f.processJob(t, runner, request)
		if partial.Status != job.Partial || partial.Progress.CommittedBatches != 4 {
			t.Fatalf("partial job = %+v", partial)
		}
		f.editor.do(t, http.MethodPost, "/databases/"+f.databaseID+"/jobs/"+partial.ID+"/cancel", nil, http.StatusAccepted)

		done := f.processJob(t, &interruptingRunner{runner: runner, after: 2}, request)
		if done.Status != job.Reverted || done.Attempts != 2 || done.Progress.CommittedBatches != 0 {
			t.Fatalf("interrupted compensation = %+v", done)
		}
		assertBalances(t, f, 0, batchRows)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_batches WHERE execution_id = $1 AND reverted_at IS NOT NULL`, 4, request.ExecutionID)
	})
}

// waitForMigrationLock waits until some session holds the target's migration
// advisory lock.
func (f *migrationFixture) waitForMigrationLock(t *testing.T) {
	t.Helper()
	key, err := migration.MigrationLockKey(f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var held bool
		if err := f.target.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted
			AND classid::bigint = 0 AND objid::bigint = $1 AND objsubid = 1)`, key).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no worker took the migration lock")
}

func (f *migrationFixture) waitForJobStatus(t *testing.T, jobID string, status job.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := f.jobs.Get(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == status {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %s", jobID, status)
}
