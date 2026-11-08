package migration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

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
	*definitionStoreStub
	frozen      bool
	freezeError error
}

func (s *executionStore) FreezeRevision(_ context.Context, databaseID, migrationID string, revision int64) error {
	s.scope(databaseID, migrationID)
	if revision != s.revision.Number {
		s.t.Fatal("revisi yang dibekukan salah")
	}
	s.frozen = true
	return s.freezeError
}
func executionFixture(t *testing.T) (*executionStore, migration.ExecutionRequest, migration.ExecutionDependencies) {
	t.Helper()
	store := &executionStore{definitionStoreStub: newDefinitionStoreStub(t)}
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
		repositoryStep{operation: "exec", statement: request.Plan.SQL},
		repositoryStep{operation: "query", statement: "INSERT INTO platform_migration_executions", rows: [][]driver.Value{executionValues(stored)}, check: func(t *testing.T, args []driver.NamedValue) {
			if args[0].Value != request.ExecutionID || args[6].Value != request.Plan.SQL || args[7].Value != request.ActorID {
				t.Fatal("history tidak menggunakan snapshot job")
			}
		}},
		repositoryStep{operation: "commit"})
	_, tx := testHistoryTransaction(t, steps...)
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
					store.freezeError = migration.ErrConflict
				case "SQL failure":
					steps = append(steps, repositoryStep{operation: "exec", statement: request.Plan.SQL, err: migration.ErrStorage})
					want = migration.ErrStorage
				}
			}
			steps = append(steps, repositoryStep{operation: "rollback"})
			_, tx := testHistoryTransaction(t, steps...)
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
			_, tx := testHistoryTransaction(t, steps...)
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

func TestExecutionAPIQueuesAndFailsClosedThroughApplicationRoutes(t *testing.T) {
	for _, alias := range []bool{false, true} {
		store, request, dependencies := executionFixture(t)
		target := testMigrationDatabase(t, actionHistorySteps(nil)...)
		service := migration.NewServiceWithExecution(executionOpener(target), store, dependencies)
		handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewEdit, false)...)
		body := map[string]any{"action": request.Plan.Action, "approval_id": request.ApprovalID}
		path := migrationAPIBase + "/definitions/" + repositoryMigrationID + "/actions"
		if alias {
			path = migrationAPIBase
			body["migration_id"] = repositoryMigrationID
		}
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, migrationRequest(http.MethodPost, path, string(data)))
		var receipt migration.QueuedAction
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusAccepted || receipt.JobID != "stored-job" || receipt.ExecutionID == "" {
			t.Fatalf("pengajuan API = %d %s", response.Code, response.Body.String())
		}
	}
	for _, scenario := range []string{"unavailable", "legacy SQL", "view-only", "CSRF", "approval", "permission"} {
		t.Run(scenario, func(t *testing.T) {
			store, request, dependencies := executionFixture(t)
			path := migrationAPIBase + "/definitions/" + repositoryMigrationID + "/actions"
			body := `{"action":"apply","approval_id":"` + request.ApprovalID + `"}`
			want := http.StatusServiceUnavailable
			authSteps := migrationAuthorizationSteps(access.ViewEdit, false)
			var targetSteps []repositoryStep
			switch scenario {
			case "unavailable":
				dependencies.Queue = nil
			case "legacy SQL":
				path = migrationAPIBase
				body = `{"name":"direct","sql":"SELECT 1"}`
				want = http.StatusBadRequest
			case "view-only":
				authSteps = migrationAuthorizationSteps(access.ViewOnly, false)
				want = http.StatusForbidden
			case "CSRF":
				authSteps = authSteps[:len(authSteps)-1]
				want = http.StatusForbidden
			case "approval":
				dependencies.Approvals = approvalCheckerFunc(func(context.Context, migration.ExecutionRequest) error { return migration.ErrApprovalRequired })
				targetSteps = actionHistorySteps(nil)
				want = http.StatusConflict
			case "permission":
				dependencies.Authorizer = actionAuthorizerFunc(func(context.Context, string, string, access.Permission) error { return access.ErrForbidden })
				want = http.StatusForbidden
			}
			target := testMigrationDatabase(t, targetSteps...)
			handler := migrationAPIHandler(t, migration.NewServiceWithExecution(executionOpener(target), store, dependencies), authSteps...)
			httpRequest := migrationRequest(http.MethodPost, path, body)
			if scenario == "CSRF" {
				httpRequest.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpRequest)
			if response.Code != want {
				t.Fatalf("API = %d %s; ingin %d", response.Code, response.Body.String(), want)
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

func TestMigrationLockKeyNormalizesDatabaseUUID(t *testing.T) {
	databaseID := "01234567-89ab-4cde-8fab-0123456789ab"
	lower, err := migration.MigrationLockKey(databaseID)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := migration.MigrationLockKey(strings.ToUpper(databaseID))
	if err != nil || lower != upper {
		t.Fatalf("kunci untuk database yang sama berbeda: %d, %d, %v", lower, upper, err)
	}
}
