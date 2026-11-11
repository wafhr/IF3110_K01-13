package migration_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

type actionDefinitionReader struct {
	t                *testing.T
	definition       migration.Migration
	revision         migration.Revision
	expectedRevision int64
	getCalls         int
	revisionCalls    int
	definitionError  error
	revisionError    error
}

func (r *actionDefinitionReader) Get(_ context.Context, databaseID, migrationID string) (migration.Migration, error) {
	r.t.Helper()
	r.getCalls++
	if databaseID != r.definition.DatabaseID || migrationID != r.definition.ID {
		r.t.Fatalf("scope definisi tidak sesuai: %q, %q", databaseID, migrationID)
	}
	return r.definition, r.definitionError
}

func (r *actionDefinitionReader) GetRevision(_ context.Context, databaseID, migrationID string, number int64) (migration.Revision, error) {
	r.t.Helper()
	r.revisionCalls++
	if databaseID != r.definition.DatabaseID || migrationID != r.definition.ID || number != r.expectedRevision {
		r.t.Fatalf("revisi yang diminta tidak sesuai: %q, %q, %d", databaseID, migrationID, number)
	}
	return r.revision, r.revisionError
}

func actionHistorySteps(history []migration.Execution) []repositoryStep {
	rows := make([][]driver.Value, 0, len(history))
	for index := len(history) - 1; index >= 0; index-- {
		rows = append(rows, executionValues(history[index]))
	}
	return append(openedHistorySteps(), repositoryStep{operation: "query",
		statement: "FROM platform_migration_executions ORDER BY sequence DESC", rows: rows, check: repositoryArguments([]any{}...)})
}

func TestPrepareActionLoadsOriginalRevision(t *testing.T) {
	for _, tc := range []struct {
		name            string
		action          migration.Action
		migrationNumber int
		history         []migration.Execution
	}{
		{"apply current revision", migration.Apply, 4, rolledBackTimeline()},
		{"rollback latest forward execution", migration.Rollback, 2, append(rolledBackTimeline(), timelineExecution(6, 2, migration.Forward))},
		{"forward original revision", migration.Forward, 2, rolledBackTimeline()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			execution := timelineExecution(1, tc.migrationNumber, migration.Apply)
			definition := timelineDefinition(execution)
			definition.CurrentRevision = 9
			revision := timelineRevision(execution)
			if tc.action == migration.Apply {
				revision.Number = definition.CurrentRevision
			}
			reader := &actionDefinitionReader{t: t, definition: definition, revision: revision, expectedRevision: revision.Number}
			target := testMigrationDatabase(t, actionHistorySteps(tc.history)...)
			service := migration.NewServiceWithRepository(func(_ context.Context, databaseID string) (*sql.DB, database.DBMS, error) {
				if databaseID != repositoryDatabaseID {
					t.Fatal("database target tidak sesuai")
				}
				return target, database.PostgreSQL, nil
			}, reader)
			plan, err := service.PrepareAction(context.Background(), repositoryDatabaseID, definition.ID, tc.action)
			if err != nil {
				t.Fatal(err)
			}
			wantSQL, err := revision.SQLFor(tc.action)
			if err != nil {
				t.Fatal(err)
			}
			last := tc.history[len(tc.history)-1]
			if plan.SQL != wantSQL || plan.Revision != revision.Number || plan.BaseExecutionID != last.ID || reader.getCalls != 1 || reader.revisionCalls != 1 {
				t.Fatalf("rencana memakai revisi atau history yang salah: %+v", plan)
			}
			if tc.action == migration.Rollback && plan.RevertsExecutionID != last.ID {
				t.Fatalf("rollback harus merujuk execution forward terakhir: %+v", plan)
			}
		})
	}
}

func TestPrepareActionRejectsInvalidRequestsBeforeDependencies(t *testing.T) {
	reader := &actionDefinitionReader{t: t}
	service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		t.Fatal("target dibuka untuk request tidak valid")
		return nil, "", nil
	}, reader)
	for _, tc := range []struct {
		databaseID, migrationID string
		action                  migration.Action
	}{
		{"bad", repositoryMigrationID, migration.Apply},
		{repositoryDatabaseID, "bad", migration.Apply},
		{repositoryDatabaseID, repositoryMigrationID, "bad"},
	} {
		if _, err := service.PrepareAction(context.Background(), tc.databaseID, tc.migrationID, tc.action); !errors.Is(err, migration.ErrInvalidMigration) {
			t.Fatal("request tidak valid diterima:", err)
		}
	}
	if reader.getCalls != 0 {
		t.Fatal("definisi dibaca untuk request tidak valid")
	}
	if _, err := migration.NewService(nil).PrepareAction(context.Background(), repositoryDatabaseID, repositoryMigrationID, migration.Apply); !errors.Is(err, migration.ErrStorage) {
		t.Fatal("repository yang belum tersedia tidak ditangani:", err)
	}
}

func TestPrepareActionPropagatesReadFailures(t *testing.T) {
	for _, failure := range []string{"definition", "target", "history initialization", "history read", "revision"} {
		t.Run(failure, func(t *testing.T) {
			execution := timelineExecution(1, 1, migration.Apply)
			reader := &actionDefinitionReader{t: t, definition: timelineDefinition(execution), revision: timelineRevision(execution), expectedRevision: 1}
			steps := actionHistorySteps(nil)
			want := migration.ErrStorage
			var targetError error
			switch failure {
			case "definition":
				reader.definitionError, want, steps = migration.ErrNotFound, migration.ErrNotFound, nil
			case "target":
				targetError, want, steps = context.DeadlineExceeded, context.DeadlineExceeded, nil
			case "history initialization":
				steps = []repositoryStep{{operation: "query", err: errors.New("private detail")}}
			case "history read":
				steps[len(steps)-1].err = errors.New("private detail")
			case "revision":
				reader.revisionError, want = migration.ErrNotFound, migration.ErrNotFound
			}
			target := testMigrationDatabase(t, steps...)
			service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
				if targetError != nil {
					return nil, "", targetError
				}
				return target, database.PostgreSQL, nil
			}, reader)
			_, err := service.PrepareAction(context.Background(), repositoryDatabaseID, reader.definition.ID, migration.Apply)
			if !errors.Is(err, want) || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("PrepareAction() = %v; diharapkan %v", err, want)
			}
		})
	}
}

func TestPrepareActionRejectsUnsupportedTarget(t *testing.T) {
	execution := timelineExecution(1, 1, migration.Apply)
	reader := &actionDefinitionReader{t: t, definition: timelineDefinition(execution)}
	target := testMigrationDatabase(t)
	service := migration.NewServiceWithRepository(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return target, database.MySQL, nil
	}, reader)
	if _, err := service.PrepareAction(context.Background(), repositoryDatabaseID, reader.definition.ID, migration.Apply); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("target di luar cakupan tindakan revisi diterima:", err)
	}
}

func TestPrepareActionRejectsMissingTargetOpener(t *testing.T) {
	reader := &actionDefinitionReader{t: t}
	service := migration.NewServiceWithRepository(nil, reader)
	if _, err := service.PrepareAction(context.Background(), repositoryDatabaseID, repositoryMigrationID, migration.Apply); !errors.Is(err, migration.ErrStorage) {
		t.Fatal("konektor target yang belum tersedia tidak ditangani:", err)
	}
	if reader.getCalls != 0 {
		t.Fatal("dependency yang belum lengkap digunakan untuk membaca definisi")
	}
}

func openedHistorySteps() []repositoryStep {
	steps := []repositoryStep{{operation: "query", statement: "to_regclass('platform_migration_executions')", rows: [][]driver.Value{{false}}}}
	return append(steps, historyInitializationSteps(false)...)
}

func TestExecutionHistoryReadsExistingSchemaWithReadOnlyQueries(t *testing.T) {
	steps := []repositoryStep{
		{operation: "query", statement: "to_regclass('platform_migration_executions')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "to_regclass('platform_migrations')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "SELECT EXISTS", rows: [][]driver.Value{{false}}},
		{operation: "query", statement: "ORDER BY sequence DESC"},
		{operation: "query", statement: "to_regclass('platform_migrations')", rows: [][]driver.Value{{true}}},
		{operation: "query", statement: "WHERE status <> 'success'"},
	}
	target := testMigrationDatabase(t, steps...)
	service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) {
		return target, database.PostgreSQL, nil
	})
	history, err := service.ReadExecutionHistory(context.Background(), repositoryDatabaseID, false)
	if err != nil || history.Executions == nil || history.LegacyAttempts == nil {
		t.Fatalf("pembacaan history harus memakai query read-only pada schema siap pakai: %+v, %v", history, err)
	}
}

type actionAuthorizerFunc func(context.Context, string, string, access.Permission) error

func (f actionAuthorizerFunc) AuthorizeDatabase(ctx context.Context, actor, databaseID string, permission access.Permission) error {
	return f(ctx, actor, databaseID, permission)
}

type approvalCheckerFunc func(context.Context, migration.ExecutionRequest) error

func (f approvalCheckerFunc) ValidateApproval(ctx context.Context, request migration.ExecutionRequest) error {
	return f(ctx, request)
}

type actionQueueFunc func(context.Context, migration.ExecutionRequest) (string, error)

func (f actionQueueFunc) Enqueue(ctx context.Context, request migration.ExecutionRequest) (string, error) {
	return f(ctx, request)
}

type executionStore struct {
	*actionDefinitionReader
	frozen      bool
	freezeError error
}

func (s *executionStore) FreezeRevision(_ context.Context, databaseID, migrationID string, revision int64) error {
	if databaseID != s.definition.DatabaseID || migrationID != s.definition.ID || revision != s.expectedRevision {
		s.t.Fatal("revisi yang dibekukan salah")
	}
	s.frozen = true
	return s.freezeError
}
func executionFixture(t *testing.T) (*executionStore, migration.ExecutionRequest, migration.ExecutionDependencies) {
	t.Helper()
	revision := validRevision()
	revision.MigrationID, revision.CreatedBy = repositoryMigrationID, repositoryActorID
	store := &executionStore{actionDefinitionReader: &actionDefinitionReader{
		t: t, definition: migration.Migration{ID: repositoryMigrationID, DatabaseID: repositoryDatabaseID, Name: "ubah pelanggan", CurrentRevision: 1},
		revision: revision, expectedRevision: 1,
	}}
	plan, err := migration.PlanAction(repositoryDatabaseID, nil, store.definition, store.revision, migration.Apply)
	if err != nil {
		t.Fatal(err)
	}
	request := migration.ExecutionRequest{ExecutionID: executionID(20), ApprovalID: executionID(21), ActorID: repositoryActorID, Plan: plan}
	dependencies := migration.ExecutionDependencies{
		Authorizer: actionAuthorizerFunc(func(_ context.Context, actor, databaseID string, permission access.Permission) error {
			if actor != repositoryActorID || databaseID != repositoryDatabaseID || permission != access.ViewEdit {
				t.Fatal("scope otorisasi salah")
			}
			return nil
		}),
		Approvals: approvalCheckerFunc(func(_ context.Context, value migration.ExecutionRequest) error {
			if value.ActorID != repositoryActorID || value.ApprovalID != request.ApprovalID || value.Plan != request.Plan {
				t.Fatal("snapshot approval salah")
			}
			return nil
		}),
		Queue: actionQueueFunc(func(context.Context, migration.ExecutionRequest) (string, error) { return "stored-job", nil }),
	}
	return store, request, dependencies
}
func executionOpener(db *sql.DB) migration.TargetOpener {
	return func(context.Context, string) (*sql.DB, database.DBMS, error) { return db, database.PostgreSQL, nil }
}

func TestSubmitActionQueuesApprovedSnapshotWithoutExecutingSQL(t *testing.T) {
	store, request, dependencies := executionFixture(t)
	var queued migration.ExecutionRequest
	dependencies.Queue = actionQueueFunc(func(_ context.Context, value migration.ExecutionRequest) (string, error) {
		queued = value
		return "stored-job", nil
	})
	target := testMigrationDatabase(t, actionHistorySteps(nil)...)
	service := migration.NewServiceWithExecution(executionOpener(target), store, dependencies)
	result, err := service.SubmitAction(context.Background(), request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, request.Plan.Action, request.ApprovalID)
	if err != nil || result.JobID != "stored-job" || result.ExecutionID == "" || queued.ExecutionID != result.ExecutionID || queued.Plan != request.Plan || store.frozen {
		t.Fatalf("pengajuan salah: %+v, %v; payload=%+v", result, err, queued)
	}
}

func TestSubmitActionFailsClosed(t *testing.T) {
	for _, scenario := range []string{"dependencies", "permission", "approval", "queue", "empty job", "transaction control", "invalid approval"} {
		t.Run(scenario, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			want := migration.ErrStorage
			steps := actionHistorySteps(nil)
			switch scenario {
			case "dependencies":
				dependencies.Queue = nil
				steps = nil
				want = migration.ErrExecutionUnavailable
			case "permission":
				dependencies.Authorizer = actionAuthorizerFunc(func(context.Context, string, string, access.Permission) error { return access.ErrForbidden })
				steps = nil
				want = access.ErrForbidden
			case "approval":
				dependencies.Approvals = approvalCheckerFunc(func(context.Context, migration.ExecutionRequest) error { return migration.ErrApprovalRequired })
				want = migration.ErrApprovalRequired
			case "queue":
				dependencies.Queue = actionQueueFunc(func(context.Context, migration.ExecutionRequest) (string, error) { return "", migration.ErrStorage })
			case "empty job":
				dependencies.Queue = actionQueueFunc(func(context.Context, migration.ExecutionRequest) (string, error) { return "", nil })
			case "transaction control":
				store.revision.SQLUp = "SELECT 1; \vCOMMIT; SELECT 1/0"
				want = migration.ErrInvalidMigration
			case "invalid approval":
				request.ApprovalID = ""
				steps = nil
				want = migration.ErrInvalidMigration
			}
			target := testMigrationDatabase(t, steps...)
			service := migration.NewServiceWithExecution(executionOpener(target), store, dependencies)
			result, err := service.SubmitAction(context.Background(), request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, request.Plan.Action, request.ApprovalID)
			if !errors.Is(err, want) || result != (migration.QueuedAction{}) || store.frozen {
				t.Fatalf("pengajuan gagal = %+v, %v", result, err)
			}
		})
	}
}

func executionPreparationSteps(locked bool, existing *migration.Execution) []repositoryStep {
	steps := []repositoryStep{{operation: "begin"}, {operation: "query", statement: "mode = 'ExclusiveLock'", rows: [][]driver.Value{{locked}}}}
	if !locked {
		return steps
	}
	rows := make([][]driver.Value, 0)
	if existing != nil {
		rows = append(rows, executionValues(*existing))
	}
	return append(steps, repositoryStep{operation: "query", statement: "WHERE database_id = $1 AND id = $2", rows: rows})
}

func TestPrepareExecutionRevalidatesAndRecordsInWorkerTransaction(t *testing.T) {
	store, request, dependencies := executionFixture(t)
	stored := committedExecution(20, migration.Apply)
	stored.SQL, stored.Name = request.Plan.SQL, request.Plan.Name
	steps := append(executionPreparationSteps(true, nil),
		repositoryStep{operation: "query", statement: "ORDER BY sequence DESC"},
		repositoryStep{operation: "query", statement: "pg_catalog.quote_ident(n.nspname)", rows: [][]driver.Value{{"public"}}},
		repositoryStep{operation: "exec", statement: request.Plan.SQL},
		repositoryStep{operation: "query", statement: "current_setting('search_path')", rows: [][]driver.Value{{"application"}}},
		repositoryStep{operation: "exec", statement: "set_config('search_path'", check: repositoryArguments("pg_catalog, public")},
		repositoryStep{operation: "query", statement: "INSERT INTO platform_migration_executions", rows: [][]driver.Value{executionValues(stored)}, check: func(t *testing.T, args []driver.NamedValue) {
			if args[0].Value != request.ExecutionID || args[6].Value != request.Plan.SQL || args[7].Value != request.ActorID {
				t.Fatal("history tidak menggunakan snapshot job")
			}
		}},
		repositoryStep{operation: "exec", statement: "set_config('search_path'", check: repositoryArguments("application")},
		repositoryStep{operation: "commit"})
	tx := testHistoryTransaction(t, steps...)
	service := migration.NewServiceWithExecution(nil, store, dependencies)
	prepared, err := service.PrepareExecution(context.Background(), tx, request)
	if err != nil || !store.frozen || prepared.Plan() != request.Plan {
		t.Fatalf("persiapan worker = %+v, %v", prepared, err)
	}
	if _, committed := prepared.Committed(); committed {
		t.Fatal("job baru dianggap sudah commit")
	}
	if _, err := tx.ExecContext(context.Background(), prepared.Plan().SQL); err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record(context.Background(), repositoryTime)
	if err != nil || record.ID != request.ExecutionID {
		t.Fatalf("record = %+v, %v", record, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareExecutionRejectsChangedAuthorizationAndPlan(t *testing.T) {
	for _, scenario := range []string{"missing lock", "permission revoked", "approval revoked", "stale plan", "freeze failure", "dependencies", "SQL failure"} {
		t.Run(scenario, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			steps := executionPreparationSteps(scenario != "missing lock", nil)
			want := migration.ErrConflict
			switch scenario {
			case "permission revoked":
				dependencies.Authorizer = actionAuthorizerFunc(func(context.Context, string, string, access.Permission) error { return access.ErrForbidden })
				want = access.ErrForbidden
			case "dependencies":
				dependencies.Approvals = nil
				want = migration.ErrExecutionUnavailable
			case "approval revoked", "stale plan", "freeze failure", "SQL failure":
				steps = append(steps, repositoryStep{operation: "query", statement: "ORDER BY sequence DESC"})
				switch scenario {
				case "approval revoked":
					dependencies.Approvals = approvalCheckerFunc(func(context.Context, migration.ExecutionRequest) error { return migration.ErrApprovalRequired })
					want = migration.ErrApprovalRequired
				case "stale plan":
					request.Plan.BaseExecutionID = executionID(99)
				case "freeze failure":
					steps = append(steps, repositoryStep{operation: "query", statement: "pg_catalog.quote_ident(n.nspname)", rows: [][]driver.Value{{"public"}}})
					store.freezeError = migration.ErrConflict
				case "SQL failure":
					steps = append(steps, repositoryStep{operation: "query", statement: "pg_catalog.quote_ident(n.nspname)", rows: [][]driver.Value{{"public"}}})
					steps = append(steps, repositoryStep{operation: "exec", statement: request.Plan.SQL, err: migration.ErrStorage})
					want = migration.ErrStorage
				}
			}
			steps = append(steps, repositoryStep{operation: "rollback"})
			tx := testHistoryTransaction(t, steps...)
			service := migration.NewServiceWithExecution(nil, store, dependencies)
			prepared, err := service.PrepareExecution(context.Background(), tx, request)
			if scenario == "SQL failure" && err == nil {
				_, err = tx.ExecContext(context.Background(), prepared.Plan().SQL)
			}
			if !errors.Is(err, want) {
				t.Fatalf("pemeriksaan ulang = %v; ingin %v", err, want)
			}
			if store.frozen && scenario != "freeze failure" && scenario != "SQL failure" {
				t.Fatal("definisi dibekukan sebelum lolos validasi")
			}
		})
	}
}

func TestPrepareExecutionUsesCommitProofForRedelivery(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching job", true: "different payload"}[mismatch], func(t *testing.T) {
			_, request, _ := executionFixture(t)
			existing := committedExecution(20, migration.Apply)
			existing.SQL, existing.Name = request.Plan.SQL, request.Plan.Name
			if mismatch {
				existing.SQL = "SELECT 2"
			}
			steps := append(executionPreparationSteps(true, &existing), repositoryStep{operation: "rollback"})
			tx := testHistoryTransaction(t, steps...)
			prepared, err := migration.NewService(nil).PrepareExecution(context.Background(), tx, request)
			if mismatch {
				if !errors.Is(err, migration.ErrConflict) {
					t.Fatalf("payload berbeda diterima: %v", err)
				}
				return
			}
			proof, committed := prepared.Committed()
			if err != nil || !committed || proof.ID != request.ExecutionID {
				t.Fatalf("bukti commit = %+v, %t, %v", proof, committed, err)
			}
			if _, err := prepared.Record(context.Background(), repositoryTime); !errors.Is(err, migration.ErrConflict) {
				t.Fatal("job duplikat dapat dicatat kembali")
			}
		})
	}
}

func TestQueuedAuthorizationRequiresActiveUser(t *testing.T) {
	db := testMigrationDatabase(t, repositoryStep{operation: "query", statement: "JOIN access_users u ON u.id = m.user_id AND u.is_active"})
	if err := access.NewService(db).AuthorizeDatabase(context.Background(), repositoryActorID, repositoryDatabaseID, access.ViewEdit); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("pengguna nonaktif diterima: %v", err)
	}
}

func TestWorkerHooksRejectMissingTransaction(t *testing.T) {
	_, request, _ := executionFixture(t)
	if _, err := migration.NewService(nil).PrepareExecution(context.Background(), nil, request); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	if _, err := migration.ListExecutionsInTransaction(context.Background(), nil, request.Plan.DatabaseID); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	if _, err := migration.GetExecutionInTransaction(context.Background(), nil, request.Plan.DatabaseID, request.ExecutionID); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	if _, err := migration.MigrationLockKey("invalid"); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	if _, err := (migration.PreparedExecution{}).Record(context.Background(), repositoryTime); !errors.Is(err, migration.ErrConflict) {
		t.Fatal(err)
	}
}

func TestMigrationLockKeyIsSharedAcrossTargetRegistrations(t *testing.T) {
	expected, err := migration.MigrationLockKey(repositoryDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	databaseID := "01234567-89ab-4cde-8fab-0123456789ab"
	for _, id := range []string{executionID(99), databaseID, strings.ToUpper(databaseID)} {
		key, err := migration.MigrationLockKey(id)
		if err != nil || key != expected {
			t.Fatalf("registrasi target memakai kunci berbeda: %d, %d, %v", expected, key, err)
		}
	}
}

func TestRecordSearchPathFailuresRequireTransactionRollback(t *testing.T) {
	for _, failure := range []string{"read path", "set path", "append history", "restore path"} {
		t.Run(failure, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			stored := committedExecution(20, migration.Apply)
			stored.SQL, stored.Name = request.Plan.SQL, request.Plan.Name
			steps := append(executionPreparationSteps(true, nil),
				repositoryStep{operation: "query", statement: "ORDER BY sequence DESC"},
				repositoryStep{operation: "query", statement: "pg_catalog.quote_ident(n.nspname)", rows: [][]driver.Value{{"public"}}},
				repositoryStep{operation: "exec", statement: request.Plan.SQL},
			)
			recording := []repositoryStep{
				{operation: "query", statement: "current_setting('search_path')", rows: [][]driver.Value{{"application"}}},
				{operation: "exec", statement: "set_config('search_path'", check: repositoryArguments("pg_catalog, public")},
				{operation: "query", statement: "INSERT INTO platform_migration_executions", rows: [][]driver.Value{executionValues(stored)}},
				{operation: "exec", statement: "set_config('search_path'", check: repositoryArguments("application")},
			}
			switch failure {
			case "read path":
				recording = recording[:1]
				recording[0].err = migration.ErrStorage
			case "set path":
				recording = recording[:2]
				recording[1].err = migration.ErrStorage
			case "append history":
				recording[2].err = migration.ErrStorage
			case "restore path":
				recording[3].err = migration.ErrStorage
			}
			steps = append(steps, recording...)
			steps = append(steps, repositoryStep{operation: "rollback"})
			tx := testHistoryTransaction(t, steps...)
			prepared, err := migration.NewServiceWithExecution(nil, store, dependencies).PrepareExecution(context.Background(), tx, request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(context.Background(), prepared.Plan().SQL); err != nil {
				t.Fatal(err)
			}
			result, err := prepared.Record(context.Background(), repositoryTime)
			if !errors.Is(err, migration.ErrStorage) || result != (migration.Execution{}) {
				t.Fatalf("kegagalan metadata menghasilkan %+v, %v", result, err)
			}
		})
	}
}

func TestSubmissionAuditRecordsQueuedIDsAndFailedQueue(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "rejected"}[rejected], func(t *testing.T) {
			var output bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(telemetry.NewLogger(&output, "server"))
			t.Cleanup(func() { slog.SetDefault(previous) })
			store, request, dependencies := executionFixture(t)
			failure := errors.New("fixture-private-queue-error")
			dependencies.Queue = actionQueueFunc(func(context.Context, migration.ExecutionRequest) (string, error) {
				if rejected {
					return "", failure
				}
				return "queued-job", nil
			})
			service := migration.NewServiceWithExecution(executionOpener(testMigrationDatabase(t, actionHistorySteps(nil)...)), store, dependencies)
			result, err := service.SubmitAction(context.Background(), request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, request.Plan.Action, request.ApprovalID)
			if rejected && err != failure {
				t.Fatal("hasil antrean berubah")
			}
			if !rejected && err != nil {
				t.Fatal(err)
			}
			raw := output.String()
			decoder := json.NewDecoder(&output)
			found := false
			for {
				var record map[string]any
				err := decoder.Decode(&record)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if record["event"] == "migration_submit_completed" {
					found = true
					if record["actor"] != request.ActorID || record["database_id"] != request.Plan.DatabaseID || record["migration_id"] != request.Plan.MigrationID || (record["status"] == "failed") != rejected {
						t.Fatal("scope atau hasil audit pengajuan tidak sesuai")
					}
					if !rejected && (record["job_id"] != result.JobID || record["execution_id"] != result.ExecutionID) {
						t.Fatal("ID job/eksekusi tidak sesuai hasil antrean")
					}
				}
			}
			if !found || strings.Contains(raw, "fixture-private-queue-error") || strings.Contains(raw, request.Plan.SQL) {
				t.Fatal("audit pengajuan hilang atau mencatat payload sensitif")
			}
		})
	}
}

func TestSubmitActionPersistsProducerTraceAfterApproval(t *testing.T) {
	provider, err := telemetry.NewTracerProvider(io.Discard, "server")
	if err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider.RegisterSpanProcessor(recorder)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	store, request, dependencies := executionFixture(t)
	approved := false
	dependencies.Approvals = approvalCheckerFunc(func(ctx context.Context, value migration.ExecutionRequest) error {
		approved = true
		if value.Plan != request.Plan || value.TraceContext != (telemetry.TraceContext{}) {
			t.Fatal("metadata trace mengubah pemeriksaan approval")
		}
		return nil
	})
	var payload []byte
	var producerID trace.SpanID
	dependencies.Queue = actionQueueFunc(func(ctx context.Context, value migration.ExecutionRequest) (string, error) {
		if !approved {
			t.Fatal("enqueue mendahului approval")
		}
		expected := telemetry.CaptureTraceContext(ctx)
		if value.TraceContext != expected || expected.TraceParent == "" {
			t.Fatal("parent producer tidak masuk payload")
		}
		producerID = trace.SpanContextFromContext(ctx).SpanID()
		payload, err = json.Marshal(value)
		return "stored-job", err
	})
	service := migration.NewServiceWithExecution(executionOpener(testMigrationDatabase(t, actionHistorySteps(nil)...)), store, dependencies)
	ctx, requestSpan := provider.Tracer("http-test").Start(context.Background(), "request")
	result, err := service.SubmitAction(ctx, request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, request.Plan.Action, request.ApprovalID)
	requestSpan.End()
	if err != nil {
		t.Fatal(err)
	}
	var restored migration.ExecutionRequest
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	parent := trace.SpanContextFromContext(telemetry.RestoreTraceContext(context.Background(), restored.TraceContext))
	if parent.TraceID() != requestSpan.SpanContext().TraceID() || parent.SpanID() != producerID || restored.ExecutionID != result.ExecutionID || restored.Plan != request.Plan {
		t.Fatal("payload persisten kehilangan korelasi atau snapshot")
	}
	found := false
	for _, span := range recorder.Ended() {
		if span.Name() == "migration_enqueue" {
			found = true
			if span.SpanKind() != trace.SpanKindProducer || span.SpanContext().SpanID() != producerID {
				t.Fatal("kind atau ID span producer salah")
			}
		}
	}
	if !found {
		t.Fatal("span producer tidak diakhiri")
	}
}

func TestSubmitActionRejectsTransactionControl(t *testing.T) {
	for _, statement := range []string{
		"COMMIT", "SELECT 1; COMMIT AND CHAIN", "SELECT 'literal; COMMIT'; /* komentar */ ROLLBACK",
		"DO $body$ BEGIN PERFORM 1; END $body$; COMMIT", "SELECT 1; /* luar /* dalam */ selesai */ SAVEPOINT a",
		"SET LOCAL TRANSACTION READ ONLY", "SELECT 1; SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE",
	} {
		t.Run(statement, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			store.revision.SQLUp = statement
			service := migration.NewServiceWithExecution(executionOpener(testMigrationDatabase(t, actionHistorySteps(nil)...)), store, dependencies)
			if _, err := service.SubmitAction(context.Background(), request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, migration.Apply, request.ApprovalID); !errors.Is(err, migration.ErrInvalidMigration) {
				t.Fatal("kendali transaksi diterima", err)
			}
		})
	}
}

func TestSubmitActionPreservesTransactionWordsInsideSQLValues(t *testing.T) {
	for _, statement := range []string{
		"SELECT 'literal; COMMIT'", "SELECT 'don''t; ROLLBACK'",
		"DO $body$ BEGIN PERFORM 1; END $body$; SELECT 1",
		"SELECT 1 /* luar /* COMMIT */ selesai */; -- ROLLBACK\nSELECT 2",
	} {
		t.Run(statement, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			store.revision.SQLUp = statement
			dependencies.Approvals = approvalCheckerFunc(func(context.Context, migration.ExecutionRequest) error { return migration.ErrApprovalRequired })
			service := migration.NewServiceWithExecution(executionOpener(testMigrationDatabase(t, actionHistorySteps(nil)...)), store, dependencies)
			if _, err := service.SubmitAction(context.Background(), request.ActorID, request.Plan.DatabaseID, request.Plan.MigrationID, migration.Apply, request.ApprovalID); !errors.Is(err, migration.ErrApprovalRequired) {
				t.Fatal("SQL valid tertolak sebelum pemeriksaan approval", err)
			}
		})
	}
}
