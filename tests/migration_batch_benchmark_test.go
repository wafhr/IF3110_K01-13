//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

const benchmarkBatchSize = 10000

// TestMigrationBatchBenchmarkIntegration times one full-table UPDATE and the
// same change as a batch migration on scripts/seed-batch-data.sql. It runs only
// when BATCH_BENCHMARK=1 because seeding and updating take tens of seconds.
func TestMigrationBatchBenchmarkIntegration(t *testing.T) {
	if os.Getenv("BATCH_BENCHMARK") != "1" {
		t.Skip("set BATCH_BENCHMARK=1 to run the batch dataset benchmark")
	}
	f := newMigrationFixture(t)
	seed, err := os.ReadFile("../scripts/seed-batch-data.sql")
	if err != nil {
		t.Fatal(err)
	}
	seeded := time.Now()
	if _, err := f.target.Exec(string(seed)); err != nil {
		t.Fatalf("seed batch dataset: %v", err)
	}
	t.Logf("seeded dataset in %s", time.Since(seeded).Round(time.Millisecond))
	execFixture(t, f.target, `GRANT SELECT, UPDATE ON batch_benchmark_accounts TO `+pgx.Identifier{f.createdRole}.Sanitize())
	var rows int64
	if err := f.target.QueryRow(`SELECT count(*) FROM batch_benchmark_accounts`).Scan(&rows); err != nil {
		t.Fatal(err)
	}

	regular := timeRolledBackUpdate(t, f)
	t.Logf("regular UPDATE of %d rows in one transaction: %s", rows, regular.Round(time.Millisecond))
	if regular <= time.Second {
		t.Errorf("a full-table UPDATE took %s; the dataset must make it exceed one second", regular)
	}

	var result struct {
		Migration migration.Migration `json:"migration"`
	}
	decode(t, f.editor.do(t, http.MethodPost, f.migrationPath()+"/definitions", map[string]any{
		"name":     "benchmark balance raise",
		"sql_up":   "UPDATE batch_benchmark_accounts SET balance_cents = balance_cents + 100, updated_at = now() WHERE id BETWEEN $1 AND $2",
		"sql_down": "UPDATE batch_benchmark_accounts SET balance_cents = balance_cents - 100, updated_at = now() WHERE id BETWEEN $1 AND $2",
		"batch":    map[string]any{"table": "batch_benchmark_accounts", "key_column": "id", "size": benchmarkBatchSize},
	}, http.StatusCreated), &result)
	request := f.queueAction(t, result.Migration, migration.Apply)
	started := time.Now()
	done := f.processJobWithin(t, f.workerExecutor(t, &atomic.Int32{}), request, 10*time.Minute)
	batched := time.Since(started)
	if done.Status != job.Succeeded || done.Progress.CommittedRows != rows {
		t.Fatalf("batch job = %+v", done)
	}
	t.Logf("batch migration of %d rows in %d batches of %d: %s", rows, done.Progress.CommittedBatches, benchmarkBatchSize, batched.Round(time.Millisecond))
	assertCount(t, f.target, `SELECT count(*) FROM batch_benchmark_accounts WHERE balance_cents = (id * 37) % 1000000 + 100`, int(rows))
}

func timeRolledBackUpdate(t *testing.T, f *migrationFixture) time.Duration {
	t.Helper()
	tx, err := f.target.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	started := time.Now()
	if _, err := tx.Exec(`UPDATE batch_benchmark_accounts SET balance_cents = balance_cents + 100, updated_at = now()`); err != nil {
		t.Fatal(err)
	}
	return time.Since(started)
}
