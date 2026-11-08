//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestMigrationHistoryIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	up := "\n-- Pertahankan SQL asli\nCREATE TABLE migration_history_rows (id int PRIMARY KEY); INSERT INTO migration_history_rows VALUES (1);\n"
	definition := f.createMigration(t, "history berulang", up, "DROP TABLE migration_history_rows")
	applied := f.runAction(t, definition, migration.Apply)
	rollback := f.runAction(t, definition, migration.Rollback)
	forward := f.runAction(t, definition, migration.Forward)
	secondRollback := f.runAction(t, definition, migration.Rollback)
	secondForward := f.runAction(t, definition, migration.Forward)
	if rollback.RevertsExecutionID != applied.ID || secondRollback.RevertsExecutionID != forward.ID {
		t.Fatal("referensi execution asal pada rollback tidak sesuai")
	}
	full := f.executionHistory(t, false)
	visible := f.executionHistory(t, true)
	if len(full.Executions) != 5 || len(full.LegacyAttempts) != 0 || len(visible.Executions) != 1 || visible.Executions[0].ID != secondForward.ID {
		t.Fatalf("filter history salah: full=%+v, visible=%+v", full, visible)
	}
	seen := make(map[string]bool)
	for index, execution := range full.Executions {
		if seen[execution.ID] || execution.Status != migration.Success || execution.FinishedAt == nil || execution.ExecutedBy != f.actorID || execution.Revision != 1 {
			t.Fatalf("identitas history salah: %+v", execution)
		}
		if index > 0 && execution.Sequence >= full.Executions[index-1].Sequence {
			t.Fatal("history tidak berurutan dari commit terbaru")
		}
		if execution.Action != migration.Rollback && execution.SQL != up {
			t.Fatal("SQL asli berubah dalam history")
		}
		seen[execution.ID] = true
	}
	var detail struct {
		Execution migration.Execution `json:"execution"`
	}
	decode(t, f.reader.do(t, "GET", f.migrationPath()+"/history/"+forward.ID, nil, 200), &detail)
	if detail.Execution.ID != forward.ID || detail.Execution.SQL != up {
		t.Fatal("detail execution hilang setelah Hide rollback")
	}
	for _, statement := range []string{
		`UPDATE platform_migration_executions SET name='diubah'`,
		`DELETE FROM platform_migration_executions`,
		`TRUNCATE platform_migration_executions`,
	} {
		_, err := f.target.Exec(statement)
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
			t.Fatalf("history append-only menerima %s: %v", statement, err)
		}
	}
	if len(f.executionHistory(t, false).Executions) != 5 {
		t.Fatal("pemeriksaan append-only mengubah history")
	}
}

func TestMigrationHistoryTransactionIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	base := f.createMigration(t, "history atomik", "CREATE TABLE migration_atomic_rows (id int PRIMARY KEY)", "DROP TABLE migration_atomic_rows")
	f.runAction(t, base, migration.Apply)
	pending := f.createMigration(t, "batalkan commit", "SET LOCAL search_path TO pg_catalog; INSERT INTO public.migration_atomic_rows VALUES (1)", "DELETE FROM public.migration_atomic_rows WHERE id=1")
	request := f.queueAction(t, pending, migration.Apply)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, _, err := f.openTarget(ctx, f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := f.migrations.PrepareExecution(ctx, tx, request); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("transaksi tanpa lock diterima: %v", err)
	}
	key, err := migration.MigrationLockKey(f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	prepared, err := f.migrations.PrepareExecution(ctx, tx, request)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, prepared.Plan().SQL); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Record(ctx, started); err != nil {
		t.Fatal(err)
	}
	var searchPath string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('search_path')`).Scan(&searchPath); err != nil {
		t.Fatal(err)
	}
	if searchPath != "pg_catalog" {
		t.Fatalf("pengaturan SQL migration berubah sesudah Record: %q", searchPath)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_atomic_rows`, 0)
	if _, err := f.migrations.GetExecution(ctx, f.databaseID, request.ExecutionID); !errors.Is(err, migration.ErrNotFound) {
		t.Fatalf("history transaksi rollback tersimpan: %v", err)
	}
	executed, err := f.executeJob(request)
	if err != nil || executed.ID != request.ExecutionID {
		t.Fatalf("percobaan ulang setelah rollback = %+v, %v", executed, err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_atomic_rows`, 1)
}

func TestMigrationLegacyHistoryIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	legacySQL := "CREATE TABLE migration_legacy_rows (id int PRIMARY KEY)"
	success, err := f.legacyMigrations.Execute(ctx, f.databaseID, f.actorID, "legacy sukses", legacySQL)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := f.legacyMigrations.Execute(ctx, f.databaseID, f.actorID, "legacy gagal", "INSERT INTO migration_legacy_rows VALUES (1); SELECT 1/0")
	if err == nil || failed.Status != migration.Failed {
		t.Fatalf("fixture legacy gagal = %+v, %v", failed, err)
	}
	first, second := f.executionHistory(t, false), f.executionHistory(t, false)
	if len(first.Executions) != 1 || len(second.Executions) != 1 || first.Executions[0].ID != second.Executions[0].ID {
		t.Fatal("impor legacy menghasilkan execution duplikat")
	}
	imported := first.Executions[0]
	if imported.LegacyVersion == nil || *imported.LegacyVersion != success.Version || imported.MigrationID != "" || imported.Revision != 0 || imported.SQL != legacySQL {
		t.Fatalf("metadata legacy salah: %+v", imported)
	}
	if len(first.LegacyAttempts) != 1 || first.LegacyAttempts[0].Status != migration.Failed || first.LegacyAttempts[0].Version != failed.Version {
		t.Fatal("attempt legacy bercampur dengan history commit")
	}
	definition := f.createMigration(t, "definisi sesudah legacy", "SELECT 1", "SELECT 2")
	if _, err := f.migrations.PrepareAction(ctx, f.databaseID, definition.ID, migration.Rollback); !errors.Is(err, migration.ErrIrreversibleMigration) {
		t.Fatalf("legacy tanpa SQL down dapat di-rollback: %v", err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_legacy_rows`, 0)
}

func TestMigrationHistorySearchPathIntegration(t *testing.T) {
	for _, metadataSchema := range []string{"public", "History / Data"} {
		t.Run(metadataSchema, func(t *testing.T) {
			f := newMigrationFixture(t)
			if metadataSchema != "public" {
				execFixture(t, f.target, `CREATE SCHEMA `+pgx.Identifier{metadataSchema}.Sanitize()+` AUTHORIZATION `+pgx.Identifier{f.createdRole}.Sanitize())
				execFixture(t, f.target, `ALTER ROLE `+pgx.Identifier{f.createdRole}.Sanitize()+` IN DATABASE `+pgx.Identifier{f.settings.DatabaseName}.Sanitize()+` SET search_path TO `+pgx.Identifier{metadataSchema}.Sanitize()+`, public`)
			}
			execFixture(t, f.target, `CREATE SCHEMA "Application / Data" AUTHORIZATION `+pgx.Identifier{f.createdRole}.Sanitize())
			// Tabel bernama sama pada schema aplikasi harus terpisah dari metadata.
			execFixture(t, f.target, `CREATE TABLE "Application / Data".platform_migration_executions (marker int); INSERT INTO "Application / Data".platform_migration_executions VALUES (99)`)
			up := `SET LOCAL search_path TO "Application / Data"; CREATE TABLE accounts (id int PRIMARY KEY); INSERT INTO accounts VALUES (1)`
			down := `SET LOCAL search_path TO "Application / Data"; DROP TABLE accounts`
			definition := f.createMigration(t, "migration lintas schema", up, down)
			applied := f.runAction(t, definition, migration.Apply)
			assertCount(t, f.target, `SELECT count(*) FROM "Application / Data".accounts`, 1)
			rolledBack := f.runAction(t, definition, migration.Rollback)
			if rolledBack.RevertsExecutionID != applied.ID {
				t.Fatal("rollback kehilangan referensi asal")
			}
			assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE schemaname='Application / Data' AND tablename='accounts'`, 0)
			forwarded := f.runAction(t, definition, migration.Forward)
			if forwarded.SQL != up || forwarded.ID == applied.ID {
				t.Fatal("forward mengubah snapshot migration")
			}
			assertCount(t, f.target, `SELECT count(*) FROM "Application / Data".accounts`, 1)
			assertCount(t, f.target, `SELECT count(*) FROM `+pgx.Identifier{metadataSchema, "platform_migration_executions"}.Sanitize(), 3)
			assertCount(t, f.target, `SELECT count(*) FROM "Application / Data".platform_migration_executions WHERE marker=99`, 1)
			if len(f.executionHistory(t, false).Executions) != 3 {
				t.Fatal("history lintas schema tidak lengkap")
			}
		})
	}
}

func TestMigrationTargetLockIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	duplicate, err := f.registry.Create(ctx, "Alias target migration", f.settings)
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := migration.MigrationLockKey(f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := migration.MigrationLockKey(duplicate.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstPool, _, err := f.openTarget(ctx, f.databaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer firstPool.Close()
	secondPool, _, err := f.openTarget(ctx, duplicate.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer secondPool.Close()
	first, err := firstPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := secondPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var locked bool
	if err := first.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, firstKey).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock pertama gagal: %t, %v", locked, err)
	}
	defer first.ExecContext(context.Background(), `SELECT pg_advisory_unlock_all()`)
	if err := second.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, secondKey).Scan(&locked); err != nil || locked {
		t.Fatalf("alias target memperoleh lock bersamaan: %t, %v", locked, err)
	}
	// Database platform adalah database fisik lain pada server PostgreSQL yang sama.
	other, err := f.platform.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, firstKey).Scan(&locked); err != nil || !locked {
		t.Fatalf("database lain ikut terhalang: %t, %v", locked, err)
	}
	defer other.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, firstKey)
	if err := first.QueryRowContext(ctx, `SELECT pg_advisory_unlock($1)`, firstKey).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock pertama gagal dilepas: %t, %v", locked, err)
	}
	if err := second.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, secondKey).Scan(&locked); err != nil || !locked {
		t.Fatalf("alias tetap terhalang sesudah lock dilepas: %t, %v", locked, err)
	}
	defer second.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, secondKey)
}
