//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestMigrationRollbackForwardIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	originalUp := "\n-- Buat data pengujian\nCREATE TABLE migration_rows (id int PRIMARY KEY); INSERT INTO migration_rows VALUES (1);\n"
	first := f.createMigration(t, "buat data", "SELECT 1", "SELECT 2")
	path := f.migrationPath() + "/definitions/" + first.ID
	revision := map[string]any{"name": "buat data", "sql_up": originalUp, "sql_down": "DROP TABLE migration_rows", "expected_revision": 1}
	f.editor.do(t, "PUT", path, revision, 200)
	f.editor.do(t, "PUT", path, revision, 409)
	var oldRevision struct {
		Revision migration.Revision `json:"revision"`
	}
	decode(t, f.reader.do(t, "GET", path+"/revisions/1", nil, 200), &oldRevision)
	if oldRevision.Revision.SQLUp != "SELECT 1" {
		t.Fatal("revisi sebelumnya berubah")
	}
	applyJob := f.queueAction(t, first, migration.Apply)
	assertCount(t, f.target, `SELECT count(*) FROM pg_tables WHERE tablename='migration_rows'`, 0)
	applied, err := f.executeJob(applyJob)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision != 2 || applied.SQL != originalUp || applied.ExecutedBy != f.actorID {
		t.Fatalf("snapshot apply berubah: %+v", applied)
	}
	duplicate, err := f.executeJob(applyJob)
	if err != nil || duplicate.ID != applied.ID || duplicate.Sequence != applied.Sequence {
		t.Fatalf("pengiriman ulang = %+v, %v", duplicate, err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_rows`, 1)
	second := f.createMigration(t, "baris kedua", "INSERT INTO migration_rows VALUES (2)", "DELETE FROM migration_rows WHERE id=2")
	third := f.createMigration(t, "baris ketiga", "INSERT INTO migration_rows VALUES (3)", "DELETE FROM migration_rows WHERE id=3")
	secondApply := f.runAction(t, second, migration.Apply)
	thirdApply := f.runAction(t, third, migration.Apply)
	if _, err := f.migrations.PrepareAction(ctx, f.databaseID, first.ID, migration.Rollback); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("rollback selain terakhir diterima: %v", err)
	}
	f.editor.do(t, "POST", path+"/actions", map[string]any{"action": "rollback", "approval_id": applyJob.ApprovalID}, 409)
	thirdRollback := f.runAction(t, third, migration.Rollback)
	secondRollback := f.runAction(t, second, migration.Rollback)
	if thirdRollback.RevertsExecutionID != thirdApply.ID || secondRollback.RevertsExecutionID != secondApply.ID {
		t.Fatal("rollback kehilangan execution ID asal")
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_rows`, 1)
	if _, err := f.migrations.PrepareAction(ctx, f.databaseID, third.ID, migration.Forward); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("forward melompati urutan: %v", err)
	}
	secondForward := f.runAction(t, second, migration.Forward)
	thirdForward := f.runAction(t, third, migration.Forward)
	if secondForward.ID == secondApply.ID || thirdForward.ID == thirdApply.ID || secondForward.SQL != secondApply.SQL || thirdForward.SQL != thirdApply.SQL {
		t.Fatal("forward mengubah identitas atau SQL asli")
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_rows`, 3)
	revision["expected_revision"] = 2
	f.editor.do(t, "PUT", path, revision, 409)
	if _, _, err := f.definitions.Revise(ctx, f.databaseID, first.ID, f.actorID, "ubah setelah eksekusi", 2, migration.Revision{SQLUp: "SELECT 3", SQLDown: "SELECT 4"}); !errors.Is(err, migration.ErrImmutableMigration) {
		t.Fatalf("revisi setelah eksekusi = %v", err)
	}
	_, err = f.platform.ExecContext(ctx, `UPDATE migration_revisions SET sql_up='SELECT 99' WHERE migration_id=$1 AND number=2`, first.ID)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || postgresError.Code != "23514" {
		t.Fatalf("proteksi SQL immutable = %v", err)
	}
	f.runAction(t, third, migration.Rollback)
	replacement := f.createMigration(t, "jalur baru", "INSERT INTO migration_rows VALUES (4)", "DELETE FROM migration_rows WHERE id=4")
	f.runAction(t, replacement, migration.Apply)
	if _, err := f.migrations.PrepareAction(ctx, f.databaseID, third.ID, migration.Forward); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("forward jalur lama diterima: %v", err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_rows WHERE id=3`, 0)
	assertCount(t, f.target, `SELECT count(*) FROM migration_rows WHERE id=4`, 1)
}

func TestMigrationExecutionValidationIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	base := f.createMigration(t, "data dasar", "CREATE TABLE migration_checks (id int PRIMARY KEY)", "DROP TABLE migration_checks")
	f.runAction(t, base, migration.Apply)
	failed := f.createMigration(t, "gagal atomik", "INSERT INTO migration_checks VALUES (1); SELECT 1/0", "DELETE FROM migration_checks WHERE id=1")
	if _, err := f.executeJob(f.queueAction(t, failed, migration.Apply)); err == nil {
		t.Fatal("SQL gagal dianggap berhasil")
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_checks`, 0)
	frozen, err := f.definitions.Get(ctx, f.databaseID, failed.ID)
	if err != nil || frozen.FrozenAt == nil {
		t.Fatalf("attempt gagal belum membekukan revisi: %+v, %v", frozen, err)
	}
	if len(f.executionHistory(t, false).Executions) != 1 {
		t.Fatal("attempt gagal masuk ke history commit")
	}
	pending := f.createMigration(t, "pengajuan tertunda", "INSERT INTO migration_checks VALUES (2)", "DELETE FROM migration_checks WHERE id=2")
	staleJob := f.queueAction(t, pending, migration.Apply)
	changedHead := f.createMigration(t, "ubah posisi history", "INSERT INTO migration_checks VALUES (3)", "DELETE FROM migration_checks WHERE id=3")
	f.runAction(t, changedHead, migration.Apply)
	if _, err := f.executeJob(staleJob); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("snapshot usang = %v", err)
	}
	revised := f.createMigration(t, "revisi tertunda", "INSERT INTO migration_checks VALUES (4)", "DELETE FROM migration_checks WHERE id=4")
	revisedJob := f.queueAction(t, revised, migration.Apply)
	f.editor.do(t, "PUT", f.migrationPath()+"/definitions/"+revised.ID, map[string]any{
		"name": "revisi tertunda", "sql_up": "INSERT INTO migration_checks VALUES (5)", "sql_down": "DELETE FROM migration_checks WHERE id=5", "expected_revision": 1,
	}, 200)
	if _, err := f.executeJob(revisedJob); !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("revisi job berubah = %v", err)
	}
	revokedJob := f.queueAction(t, pending, migration.Apply)
	f.approvals.mu.Lock()
	delete(f.approvals.snapshots, revokedJob.ApprovalID)
	f.approvals.mu.Unlock()
	if _, err := f.executeJob(revokedJob); !errors.Is(err, migration.ErrApprovalRequired) {
		t.Fatalf("approval dicabut = %v", err)
	}
	inactiveJob := f.queueAction(t, pending, migration.Apply)
	execFixture(t, f.platform, `UPDATE access_users SET is_active=false WHERE id=$1`, f.actorID)
	if _, err := f.executeJob(inactiveJob); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("aktor nonaktif = %v", err)
	}
	execFixture(t, f.platform, `UPDATE access_users SET is_active=true WHERE id=$1`, f.actorID)
	permissionJob := f.queueAction(t, pending, migration.Apply)
	if err := access.NewService(f.platform).SetDatabasePermission(ctx, f.editorGroupID, f.databaseID, access.ViewOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeJob(permissionJob); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("permission diturunkan = %v", err)
	}
	assertCount(t, f.target, `SELECT count(*) FROM migration_checks`, 1)
	if len(f.executionHistory(t, false).Executions) != 2 {
		t.Fatal("pengajuan yang ditolak mengubah history commit")
	}
}

func TestMigrationDefinitionAccessAndIrreversibleIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	path := f.migrationPath() + "/definitions"
	input := map[string]any{"name": "irreversible", "sql_up": "CREATE TABLE migration_irreversible (id int)", "irreversible": true, "irreversible_reason": "Data asal tidak tersedia untuk pemulihan"}
	f.anonymous().do(t, "GET", path, nil, 401)
	f.platformAdmin.do(t, "GET", path, nil, 403)
	f.reader.do(t, "POST", path, input, 403)
	f.editor.do(t, "POST", path, input, 403, func(request *http.Request) { request.Header.Del("X-CSRF-Token") })
	f.editor.do(t, "POST", f.migrationPath(), map[string]string{"name": "SQL langsung", "sql": "SELECT 1"}, 400)
	var created struct {
		Migration migration.Migration `json:"migration"`
		Revision  migration.Revision  `json:"revision"`
	}
	decode(t, f.editor.do(t, "POST", path, input, 201), &created)
	if !created.Revision.Irreversible || created.Revision.IrreversibleReason != input["irreversible_reason"] || created.Revision.SQLDown != "" {
		t.Fatal("alasan irreversible tidak tersimpan")
	}
	f.runAction(t, created.Migration, migration.Apply)
	if _, err := f.migrations.PrepareAction(context.Background(), f.databaseID, created.Migration.ID, migration.Rollback); !errors.Is(err, migration.ErrIrreversibleMigration) {
		t.Fatalf("rollback irreversible = %v", err)
	}
	f.editor.do(t, "POST", path+"/"+created.Migration.ID+"/actions", map[string]string{"action": "rollback", "approval_id": "00000000-0000-4000-8000-000000000001"}, 400)
}
