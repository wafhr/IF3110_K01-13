//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

const batchRows = 5000

func TestMigrationBatchIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	runner := f.workerExecutor(t, &atomic.Int32{})
	f.seedBatchAccounts(t)

	t.Run("each range commits separately and history is recorded once", func(t *testing.T) {
		definition := f.createBatchMigration(t, "raise balances",
			"UPDATE batch_accounts SET balance_cents = balance_cents + 100 WHERE id BETWEEN $1 AND $2",
			"UPDATE batch_accounts SET balance_cents = balance_cents - 100 WHERE id BETWEEN $1 AND $2")
		request := f.queueAction(t, definition, migration.Apply)
		done := f.processJob(t, runner, request)
		want := job.Progress{CommittedBatches: 5, CommittedRows: batchRows, TotalRows: batchRows}
		if done.Status != job.Succeeded || done.Progress != want {
			t.Fatalf("batch job = %+v", done)
		}
		assertBalances(t, f, 100, batchRows)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_batches WHERE execution_id = $1`, 5, request.ExecutionID)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_batches WHERE execution_id = $1 AND batch_number = 3
			AND first_key = '2001' AND last_key = '3000' AND row_count = 1000`, 1, request.ExecutionID)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_batch_scopes WHERE execution_id = $1
			AND status = 'completed' AND scope_end = '5000' AND total_rows = 5000`, 1, request.ExecutionID)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_executions WHERE id = $1`, 1, request.ExecutionID)

		var detail struct {
			Job job.Job `json:"job"`
		}
		decode(t, f.reader.do(t, http.MethodGet, "/databases/"+f.databaseID+"/jobs/"+done.ID, nil, http.StatusOK), &detail)
		if detail.Job.Progress != want {
			t.Fatalf("job API progress = %+v", detail.Job.Progress)
		}
		if again, err := runner.Run(context.Background(), request, nil); err != nil || again.ID != request.ExecutionID {
			t.Fatalf("redelivery = %+v, %v", again, err)
		}
		assertBalances(t, f, 100, batchRows)
	})

	t.Run("rows with a NULL key are outside the batch scope", func(t *testing.T) {
		execFixture(t, f.target, `CREATE TABLE batch_vouchers (code BIGINT UNIQUE, uses INT NOT NULL DEFAULT 0);
			INSERT INTO batch_vouchers (code) SELECT code FROM generate_series(1, 2500) AS code;
			INSERT INTO batch_vouchers (code) VALUES (NULL), (NULL), (NULL)`)
		execFixture(t, f.target, `GRANT SELECT, UPDATE ON batch_vouchers TO `+pgx.Identifier{f.createdRole}.Sanitize())
		var result struct {
			Migration migration.Migration `json:"migration"`
		}
		decode(t, f.editor.do(t, http.MethodPost, f.migrationPath()+"/definitions", map[string]any{
			"name": "count voucher use", "sql_up": "UPDATE batch_vouchers SET uses = uses + 1 WHERE code BETWEEN $1 AND $2",
			"sql_down": "UPDATE batch_vouchers SET uses = uses - 1 WHERE code BETWEEN $1 AND $2",
			"batch":    map[string]any{"table": "batch_vouchers", "key_column": "code", "size": 1000},
		}, http.StatusCreated), &result)
		done := f.processJob(t, runner, f.queueAction(t, result.Migration, migration.Apply))
		if done.Status != job.Succeeded || done.Progress != (job.Progress{CommittedBatches: 3, CommittedRows: 2500, TotalRows: 2500}) {
			t.Fatalf("NULL key job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM batch_vouchers WHERE uses = 1`, 2500)
		assertCount(t, f.target, `SELECT count(*) FROM batch_vouchers WHERE code IS NULL AND uses = 0`, 3)
	})

	t.Run("failed range leaves the job partial and resume continues from the checkpoint", func(t *testing.T) {
		execFixture(t, f.target, `UPDATE batch_accounts SET note = 'poison' WHERE id = 2500`)
		definition := f.createBatchMigration(t, "add bonus",
			"UPDATE batch_accounts SET balance_cents = balance_cents + 10 / (CASE WHEN note = 'poison' THEN 0 ELSE 1 END) WHERE id BETWEEN $1 AND $2",
			"UPDATE batch_accounts SET balance_cents = balance_cents - 10 WHERE id BETWEEN $1 AND $2")
		request := f.queueAction(t, definition, migration.Apply)
		partial := f.processJob(t, runner, request)
		if partial.Status != job.Partial || partial.Progress.CommittedBatches != 2 || partial.Progress.CommittedRows != 2000 ||
			partial.LastError != "SQLSTATE 22012: division by zero" {
			t.Fatalf("partial job = %+v", partial)
		}
		assertCount(t, f.target, `SELECT count(*) FROM batch_accounts WHERE balance_cents = id + 110`, 2000)
		assertCount(t, f.target, `SELECT count(*) FROM platform_migration_executions WHERE id = $1`, 0, request.ExecutionID)
		if _, err := f.jobs.Claim(context.Background(), "probe", jobLease); err == nil {
			t.Fatal("a partial job was handed out without being resumed")
		}

		f.reader.do(t, http.MethodPost, "/databases/"+f.databaseID+"/jobs/"+partial.ID+"/resume", nil, http.StatusForbidden)
		execFixture(t, f.target, `UPDATE batch_accounts SET note = NULL WHERE id = 2500`)
		var resumed struct {
			Job job.Job `json:"job"`
		}
		decode(t, f.editor.do(t, http.MethodPost, "/databases/"+f.databaseID+"/jobs/"+partial.ID+"/resume", nil, http.StatusAccepted), &resumed)
		if resumed.Job.Status != job.Pending {
			t.Fatalf("resumed job = %+v", resumed.Job)
		}
		done := f.processJob(t, runner, request)
		if done.Status != job.Succeeded || done.Progress.CommittedBatches != 5 || done.Progress.CommittedRows != batchRows {
			t.Fatalf("resumed job = %+v", done)
		}
		assertBalances(t, f, 110, batchRows)
	})

	t.Run("interrupted attempt resumes after the last committed batch", func(t *testing.T) {
		definition := f.createBatchMigration(t, "add interest",
			"UPDATE batch_accounts SET balance_cents = balance_cents + 1 WHERE id BETWEEN $1 AND $2",
			"UPDATE batch_accounts SET balance_cents = balance_cents - 1 WHERE id BETWEEN $1 AND $2")
		request := f.queueAction(t, definition, migration.Apply)
		done := f.processJob(t, &interruptingRunner{runner: runner, after: 2}, request)
		if done.Status != job.Succeeded || done.Attempts != 2 || done.Progress.CommittedBatches != 5 {
			t.Fatalf("interrupted job = %+v", done)
		}
		assertBalances(t, f, 111, batchRows)
	})

	t.Run("rollback of a batch migration is rejected until compensation runs", func(t *testing.T) {
		definition := f.createBatchMigration(t, "last batch change",
			"UPDATE batch_accounts SET note = 'seen' WHERE id BETWEEN $1 AND $2",
			"UPDATE batch_accounts SET note = NULL WHERE id BETWEEN $1 AND $2")
		f.processJob(t, runner, f.queueAction(t, definition, migration.Apply))
		done := f.processJob(t, runner, f.queueAction(t, definition, migration.Rollback))
		if done.Status != job.Failed || done.LastError != worker.ErrUnsupportedBatchAction.Error() {
			t.Fatalf("batch rollback job = %+v", done)
		}
		assertCount(t, f.target, `SELECT count(*) FROM batch_accounts WHERE note = 'seen'`, batchRows)
	})
}

// interruptingRunner cancels the first attempt right after it reports the
// given number of committed batches, like a worker stopping between batches.
type interruptingRunner struct {
	runner *worker.Executor
	after  int64
	once   sync.Once
}

func (r *interruptingRunner) Run(ctx context.Context, request migration.ExecutionRequest, report worker.ProgressFunc) (migration.Execution, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return r.runner.Run(ctx, request, func(progress job.Progress) {
		report(progress)
		if progress.CommittedBatches >= r.after {
			r.once.Do(cancel)
		}
	})
}

func (f *migrationFixture) seedBatchAccounts(t *testing.T) {
	t.Helper()
	execFixture(t, f.target, `CREATE TABLE batch_accounts (id BIGINT PRIMARY KEY, balance_cents BIGINT NOT NULL, note TEXT);
		INSERT INTO batch_accounts SELECT id, id, NULL FROM generate_series(1, 5000) AS id`)
	execFixture(t, f.target, `GRANT SELECT, UPDATE ON batch_accounts TO `+pgx.Identifier{f.createdRole}.Sanitize())
}

func (f *migrationFixture) createBatchMigration(t *testing.T, name, up, down string) migration.Migration {
	t.Helper()
	var result struct {
		Migration migration.Migration `json:"migration"`
	}
	decode(t, f.editor.do(t, http.MethodPost, f.migrationPath()+"/definitions", map[string]any{
		"name": name, "sql_up": up, "sql_down": down,
		"batch": map[string]any{"table": "batch_accounts", "key_column": "id", "size": 1000},
	}, http.StatusCreated), &result)
	return result.Migration
}

func assertBalances(t *testing.T, f *migrationFixture, offset, rows int) {
	t.Helper()
	assertCount(t, f.target, `SELECT count(*) FROM batch_accounts WHERE balance_cents = id + $1`, rows, offset)
}
