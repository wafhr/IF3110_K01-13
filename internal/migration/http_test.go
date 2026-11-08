package migration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

const migrationAPIBase = "/databases/" + repositoryDatabaseID + "/migrations"
const migrationSessionToken = "migration-api-session"
const migrationCSRFToken = "migration-api-csrf"

type definitionStoreStub struct {
	t                *testing.T
	definition       migration.Migration
	revision         migration.Revision
	err              error
	calls            []string
	actorID          string
	expectedRevision int64
}

func newDefinitionStoreStub(t *testing.T) *definitionStoreStub {
	revision := validRevision()
	revision.MigrationID, revision.CreatedBy = repositoryMigrationID, repositoryActorID
	return &definitionStoreStub{t: t,
		definition: migration.Migration{ID: repositoryMigrationID, DatabaseID: repositoryDatabaseID, Name: "ubah pelanggan", CurrentRevision: 1},
		revision:   revision,
	}
}

func (s *definitionStoreStub) scope(databaseID, migrationID string) {
	s.t.Helper()
	if databaseID != repositoryDatabaseID || migrationID != repositoryMigrationID {
		s.t.Fatalf("scope definisi tidak sesuai: %q, %q", databaseID, migrationID)
	}
}
func (s *definitionStoreStub) List(_ context.Context, databaseID string) ([]migration.Migration, error) {
	s.scope(databaseID, repositoryMigrationID)
	s.calls = append(s.calls, "list")
	return []migration.Migration{s.definition}, s.err
}
func (s *definitionStoreStub) Get(_ context.Context, databaseID, migrationID string) (migration.Migration, error) {
	s.scope(databaseID, migrationID)
	s.calls = append(s.calls, "get")
	return s.definition, s.err
}
func (s *definitionStoreStub) GetRevision(_ context.Context, databaseID, migrationID string, number int64) (migration.Revision, error) {
	s.scope(databaseID, migrationID)
	s.calls = append(s.calls, "revision")
	if number != s.revision.Number {
		s.t.Fatalf("nomor revisi tidak sesuai: %d", number)
	}
	return s.revision, s.err
}
func (s *definitionStoreStub) Create(_ context.Context, databaseID, actorID, name string, revision migration.Revision) (migration.Migration, migration.Revision, error) {
	s.scope(databaseID, repositoryMigrationID)
	s.calls = append(s.calls, "create")
	s.actorID = actorID
	s.definition.Name = name
	revision.MigrationID, revision.CreatedBy = repositoryMigrationID, actorID
	s.revision = revision
	return s.definition, revision, s.err
}
func (s *definitionStoreStub) Revise(_ context.Context, databaseID, migrationID, actorID, name string, expectedRevision int64, revision migration.Revision) (migration.Migration, migration.Revision, error) {
	s.scope(databaseID, migrationID)
	s.calls = append(s.calls, "revise")
	s.actorID, s.expectedRevision = actorID, expectedRevision
	s.definition.Name, s.definition.CurrentRevision = name, revision.Number
	revision.MigrationID, revision.CreatedBy = migrationID, actorID
	s.revision = revision
	return s.definition, revision, s.err
}

func migrationAuthorizationSteps(permission access.Permission, admin bool) []repositoryStep {
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte(migrationSessionToken))
	permissionRows := make([][]driver.Value, 0)
	if permission != "" {
		permissionRows = append(permissionRows, []driver.Value{string(permission)})
	}
	return []repositoryStep{
		{operation: "begin"},
		{operation: "query", statement: "FROM access_sessions s JOIN access_users u", rows: [][]driver.Value{{
			hash[:], true, nil, nil, migrationCSRFToken, now, now, now.Add(time.Hour), repositoryActorID, "editor", admin, true, now,
		}}},
		{operation: "exec", statement: "UPDATE access_sessions SET last_seen_at"},
		{operation: "commit"},
		{operation: "query", statement: "bool_or(p.permission = 'view_edit')", rows: permissionRows},
	}
}

func migrationAPIHandler(t *testing.T, service *migration.Service, steps ...repositoryStep) http.Handler {
	t.Helper()
	auth := access.NewHTTPHandler(access.NewService(testMigrationDatabase(t, steps...)), false)
	viewer := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("API migration masuk ke viewer") })
	return app.RoutesWithModulesAndFrontend(auth, nil, viewer, migration.NewHTTPHandler(service, auth), nil)
}

func migrationRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
	request.AddCookie(&http.Cookie{Name: "platform_session", Value: migrationSessionToken})
	request.Header.Set("X-CSRF-Token", migrationCSRFToken)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func definitionRequestBody(t *testing.T, expectedRevision int64) string {
	t.Helper()
	input := map[string]any{"name": " ubah pelanggan ", "sql_up": validRevision().SQLUp, "sql_down": validRevision().SQLDown}
	if expectedRevision != 0 {
		input["expected_revision"] = expectedRevision
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDefinitionAPIReadsThroughApplicationRoutes(t *testing.T) {
	for _, suffix := range []string{"", "/" + repositoryMigrationID, "/" + repositoryMigrationID + "/revisions/1"} {
		t.Run(suffix, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			service := migration.NewServiceWithRepository(nil, store)
			handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewOnly, false)...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/definitions"+suffix, ""))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), repositoryMigrationID) {
				t.Fatalf("GET %s = %d %s", suffix, response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("header keamanan API tidak tersedia")
			}
		})
	}
}

func TestDefinitionAPIWritesUseSessionActorAndExpectedRevision(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			service := migration.NewServiceWithRepository(nil, store)
			handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewEdit, false)...)
			path, expected, wantStatus := migrationAPIBase+"/definitions", int64(0), http.StatusCreated
			if method == http.MethodPut {
				path += "/" + repositoryMigrationID
				expected, wantStatus = 1, http.StatusOK
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(method, path, definitionRequestBody(t, expected)))
			if response.Code != wantStatus || store.actorID != repositoryActorID || store.definition.Name != "ubah pelanggan" || store.revision.SQLUp != validRevision().SQLUp {
				t.Fatalf("%s = %d %s; actor=%q", method, response.Code, response.Body.String(), store.actorID)
			}
			if method == http.MethodPost && response.Header().Get("Location") != migrationAPIBase+"/definitions/"+repositoryMigrationID {
				t.Fatal("Location definisi baru tidak sesuai")
			}
			if method == http.MethodPut && (store.expectedRevision != 1 || store.revision.Number != 2) {
				t.Fatal("expected_revision tidak diteruskan dengan benar")
			}
		})
	}
}

func TestMigrationAPIDeniesMissingSessionGrantAndCSRF(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		permission access.Permission
		admin      bool
		cookie     bool
		csrf       bool
		want       int
	}{
		{"anonymous", http.MethodGet, access.ViewOnly, false, false, true, http.StatusUnauthorized},
		{"no grant", http.MethodGet, "", false, true, true, http.StatusForbidden},
		{"admin without grant", http.MethodGet, "", true, true, true, http.StatusForbidden},
		{"view-only write", http.MethodPost, access.ViewOnly, false, true, true, http.StatusForbidden},
		{"missing CSRF", http.MethodPost, access.ViewEdit, false, true, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			steps := migrationAuthorizationSteps(tc.permission, tc.admin)
			if !tc.cookie {
				steps = nil
			} else if !tc.csrf {
				steps = steps[:len(steps)-1]
			}
			handler := migrationAPIHandler(t, migration.NewServiceWithRepository(nil, store), steps...)
			request := migrationRequest(tc.method, migrationAPIBase+"/definitions", definitionRequestBody(t, 0))
			if !tc.cookie {
				request.Header.Del("Cookie")
			}
			if !tc.csrf {
				request.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || len(store.calls) != 0 {
				t.Fatalf("akses ditolak dengan status yang salah: %d %s; calls=%v", response.Code, response.Body.String(), store.calls)
			}
		})
	}
}

func TestDefinitionAPIRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name, method, body, contentType string
		want                            int
	}{
		{"malformed", http.MethodPost, `{`, "application/json", 400},
		{"null", http.MethodPost, `null`, "application/json", 400},
		{"unknown actor", http.MethodPost, `{"name":"x","sql_up":"SELECT 1","sql_down":"SELECT 2","created_by":"spoofed"}`, "application/json", 400},
		{"multiple values", http.MethodPost, definitionRequestBody(t, 0) + ` {}`, "application/json", 400},
		{"missing down", http.MethodPost, `{"name":"x","sql_up":"SELECT 1"}`, "application/json", 400},
		{"irreversible without reason", http.MethodPost, `{"name":"x","sql_up":"SELECT 1","irreversible":true}`, "application/json", 400},
		{"missing expected revision", http.MethodPut, definitionRequestBody(t, 0), "application/json", 400},
		{"revision overflow", http.MethodPut, `{"name":"x","sql_up":"SELECT 1","sql_down":"SELECT 2","expected_revision":9223372036854775807}`, "application/json", 400},
		{"unsupported media", http.MethodPost, definitionRequestBody(t, 0), "text/plain", 415},
		{"body limit", http.MethodPost, `{"name":"` + strings.Repeat("a", (1<<20)+1) + `"}`, "application/json", 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			handler := migrationAPIHandler(t, migration.NewServiceWithRepository(nil, store), migrationAuthorizationSteps(access.ViewEdit, false)...)
			path := migrationAPIBase + "/definitions"
			if tc.method == http.MethodPut {
				path += "/" + repositoryMigrationID
			}
			request := migrationRequest(tc.method, path, tc.body)
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || len(store.calls) != 0 {
				t.Fatalf("input tidak valid diterima: %d %s; calls=%v", response.Code, response.Body.String(), store.calls)
			}
		})
	}
}

func TestDefinitionAPIMapsErrorsWithoutLeakingDetails(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"missing", migration.ErrNotFound, 404},
		{"stale revision", migration.ErrConflict, 409},
		{"frozen revision", migration.ErrImmutableMigration, 409},
		{"invalid", migration.ErrInvalidMigration, 400},
		{"deadline", context.DeadlineExceeded, 504},
		{"canceled", context.Canceled, 408},
		{"storage", errors.New("private connection password"), 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			store.err = tc.err
			handler := migrationAPIHandler(t, migration.NewServiceWithRepository(nil, store), migrationAuthorizationSteps(access.ViewOnly, false)...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/definitions/"+repositoryMigrationID, ""))
			if response.Code != tc.want || strings.Contains(response.Body.String(), "private connection password") {
				t.Fatalf("pemetaan error tidak aman: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestHistoryAPIShowsFilteredExecutionsAndLegacyAttempts(t *testing.T) {
	original := committedExecution(1, migration.Apply)
	rollback := committedExecution(2, migration.Rollback)
	rollback.RevertsExecutionID = original.ID
	forward := committedExecution(3, migration.Forward)
	steps := openedHistorySteps()
	steps = append(steps,
		repositoryStep{operation: "query", rows: [][]driver.Value{executionValues(forward), executionValues(rollback), executionValues(original)}},
		repositoryStep{operation: "query", rows: [][]driver.Value{{true}}},
		repositoryStep{operation: "query", statement: "WHERE status <> 'success'", rows: [][]driver.Value{{int64(4), "gagal", "SELECT 1/0", "failed", repositoryActorID, repositoryTime, repositoryTime}}},
	)
	target := testMigrationDatabase(t, steps...)
	service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) { return target, database.PostgreSQL, nil })
	handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewOnly, false)...)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/history?hide_rollback=true", ""))
	var result migration.ExecutionHistory
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(result.Executions) != 1 || result.Executions[0].ID != forward.ID || len(result.LegacyAttempts) != 1 || result.LegacyAttempts[0].Status != migration.Failed {
		t.Fatalf("history API tidak sesuai: %d %s", response.Code, response.Body.String())
	}
}

func TestHistoryAPIValidatesFiltersAndTargetErrors(t *testing.T) {
	for _, query := range []string{"?hide_rollback=bad", "?hide_rollback=true&hide_rollback=false", "?unknown=true", "?hide_rollback=%zz"} {
		t.Run(query, func(t *testing.T) {
			service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) {
				t.Fatal("target dibuka untuk filter invalid")
				return nil, "", nil
			})
			handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewOnly, false)...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/history"+query, ""))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("filter diterima: %d", response.Code)
			}
		})
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"permission", targetdb.ErrPermissionDenied, 403},
		{"connection", targetdb.ErrConnection, 502},
		{"authentication", targetdb.ErrAuthentication, 502},
		{"closed", targetdb.ErrClosed, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) { return nil, "", tc.err })
			handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewOnly, false)...)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/history", ""))
			if response.Code != tc.want {
				t.Fatalf("error target = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestHistoryAPIReturnsExecutionDetail(t *testing.T) {
	execution := committedExecution(1, migration.Apply)
	steps := append(openedHistorySteps(), repositoryStep{operation: "query", statement: "WHERE database_id = $1 AND id = $2", rows: [][]driver.Value{executionValues(execution)}, check: repositoryArguments(repositoryDatabaseID, execution.ID)})
	target := testMigrationDatabase(t, steps...)
	service := migration.NewService(func(context.Context, string) (*sql.DB, database.DBMS, error) { return target, database.PostgreSQL, nil })
	handler := migrationAPIHandler(t, service, migrationAuthorizationSteps(access.ViewOnly, false)...)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, migrationRequest(http.MethodGet, migrationAPIBase+"/history/"+execution.ID, ""))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), execution.ID) {
		t.Fatalf("detail history = %d %s", response.Code, response.Body.String())
	}
}

func TestEveryNewMigrationRouteRequiresAuthentication(t *testing.T) {
	for _, route := range []struct{ method, suffix string }{
		{http.MethodGet, "/definitions"},
		{http.MethodPost, "/definitions"},
		{http.MethodGet, "/definitions/" + repositoryMigrationID},
		{http.MethodPut, "/definitions/" + repositoryMigrationID},
		{http.MethodGet, "/definitions/" + repositoryMigrationID + "/revisions/1"},
		{http.MethodPost, "/definitions/" + repositoryMigrationID + "/actions"},
		{http.MethodGet, "/history"},
		{http.MethodGet, "/history/" + executionID(1)},
	} {
		t.Run(route.method+route.suffix, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			handler := migrationAPIHandler(t, migration.NewServiceWithRepository(nil, store))
			request := httptest.NewRequest(route.method, "http://example.com"+migrationAPIBase+route.suffix, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || len(store.calls) != 0 {
				t.Fatalf("route tanpa session dapat diakses: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDefinitionUpdateRejectsMissingCSRFAndViewOnlyPermission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission access.Permission
		csrf       string
	}{
		{"view-only", access.ViewOnly, migrationCSRFToken},
		{"invalid CSRF", access.ViewEdit, "invalid-csrf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newDefinitionStoreStub(t)
			steps := migrationAuthorizationSteps(tc.permission, false)
			if tc.csrf != migrationCSRFToken {
				steps = steps[:len(steps)-1]
			}
			handler := migrationAPIHandler(t, migration.NewServiceWithRepository(nil, store), steps...)
			request := migrationRequest(http.MethodPut, migrationAPIBase+"/definitions/"+repositoryMigrationID, definitionRequestBody(t, 1))
			request.Header.Set("X-CSRF-Token", tc.csrf)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || len(store.calls) != 0 {
				t.Fatalf("update tanpa izin diterima: %d", response.Code)
			}
		})
	}
}
