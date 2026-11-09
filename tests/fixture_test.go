//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	databasehttp "github.com/wafhr/IF3110_K01-13/internal/database/http"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/drivers"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/web"
)

const adminPassword = "integration-admin-password"
const memberPassword = "integration-member-password"
const targetPassword = "integration-target-password"
const quotedSchema = "Sales / Data"
const quotedTable = `Order"Items`

type fixture struct {
	admin, platform, target *sql.DB
	registry                *database.Repository
	legacyMigrations        *migration.Service
	openTarget              migration.TargetOpener
	pools                   *targetdb.Manager
	inspector               *drivers.Inspector
	server                  *httptest.Server
	settings                database.ConnectionConfig
	createdDatabases        []string
	createdRole             string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	connectionURL := os.Getenv("TEST_DATABASE_URL")
	if connectionURL == "" {
		t.Fatal("TEST_DATABASE_URL is required; see docs/database-management.md#integration-test")
	}
	config, err := pgx.ParseConfig(connectionURL)
	if err != nil {
		t.Fatal("TEST_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	if config.TLSConfig != nil {
		t.Fatal("use the dedicated integration server with sslmode=disable")
	}
	f := &fixture{admin: stdlib.OpenDB(*config)}
	t.Cleanup(func() { f.cleanup(t) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := f.admin.PingContext(ctx); err != nil {
		t.Fatal("cannot connect to TEST_DATABASE_URL PostgreSQL server")
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	prefix := "if3110_it_" + hex.EncodeToString(suffix)
	for _, name := range []string{prefix + "_platform", prefix + "_target"} {
		execFixture(t, f.admin, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
		f.createdDatabases = append(f.createdDatabases, name)
	}
	platformConfig, targetConfig := config.Copy(), config.Copy()
	platformConfig.Database, targetConfig.Database = f.createdDatabases[0], f.createdDatabases[1]
	f.platform = sql.OpenDB(telemetry.SQLConnector(stdlib.GetConnector(*platformConfig), "postgresql", "platform", platformConfig.Database))
	f.target = stdlib.OpenDB(*targetConfig)
	role := prefix + "_reader"
	execFixture(t, f.admin, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD '"+targetPassword+"'")
	f.createdRole = role
	execFixture(t, f.admin, "GRANT CONNECT ON DATABASE "+pgx.Identifier{targetConfig.Database}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
	seedTarget(t, f.target, role)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	f.registry, err = database.NewRepository(f.platform, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.registry.Initialize(ctx); err != nil {
		t.Fatalf("initialize registry: %v", err)
	}
	authService := access.NewService(f.platform)
	if err := authService.Initialize(ctx, "admin", adminPassword); err != nil {
		t.Fatalf("initialize access control: %v", err)
	}
	connector, err := drivers.NewConnector()
	if err != nil {
		t.Fatal(err)
	}
	f.pools, err = targetdb.NewManager(f.registry, connector)
	if err != nil {
		t.Fatal(err)
	}
	f.inspector = drivers.NewInspector(f.registry, f.pools)
	f.openTarget = func(ctx context.Context, id string) (*sql.DB, database.DBMS, error) {
		connection, err := f.registry.GetConnectionConfig(ctx, id)
		if err != nil {
			return nil, "", err
		}
		db, err := connector.Open(ctx, connection)
		return db, connection.DBMS, err
	}
	migrations := migration.NewService(f.openTarget)
	f.legacyMigrations = migrations
	auth := access.NewHTTPHandler(authService, false)
	f.server = httptest.NewServer(telemetry.HTTPHandler(app.RoutesWithModulesAndFrontend(auth,
		databasehttp.NewHandler(f.registry, f.pools, auth),
		databasehttp.NewViewerHandler(f.registry, f.inspector, auth),
		migration.NewHTTPHandler(migrations, auth), web.NewHandler(authService, auth))))
	f.settings = database.ConnectionConfig{Host: config.Host, Port: int(config.Port), DatabaseName: targetConfig.Database,
		DBMS: database.PostgreSQL, Username: role, Password: targetPassword, SSLMode: "disable"}
	return f
}

func (f *fixture) cleanup(t *testing.T) {
	t.Helper()
	if f.server != nil {
		f.server.Close()
	}
	if f.pools != nil {
		if err := f.pools.Close(); err != nil {
			t.Error("failed to close target pools")
		}
	}
	for _, db := range []*sql.DB{f.platform, f.target} {
		if db != nil {
			_ = db.Close()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, name := range f.createdDatabases {
		if !strings.HasPrefix(name, "if3110_it_") {
			t.Error("refusing to clean a database outside the integration prefix")
			continue
		}
		if _, err := f.admin.ExecContext(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("failed to remove disposable database %s", name)
		}
	}
	if f.createdRole != "" && strings.HasPrefix(f.createdRole, "if3110_it_") {
		if _, err := f.admin.ExecContext(ctx, "DROP ROLE "+pgx.Identifier{f.createdRole}.Sanitize()); err != nil {
			t.Error("failed to remove disposable target role")
		}
	}
	_ = f.admin.Close()
}

func seedTarget(t *testing.T, db *sql.DB, role string) {
	t.Helper()
	execFixture(t, db, `
CREATE SCHEMA "Sales / Data";
CREATE TABLE public.customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
INSERT INTO public.customers VALUES (1, 'First customer');
CREATE TABLE "Sales / Data"."Order""Items" (
    id BIGINT GENERATED BY DEFAULT AS IDENTITY,
    customer_id INTEGER,
    amount NUMERIC(30,10) NOT NULL DEFAULT 0,
    note TEXT,
    payload JSONB,
    doubled NUMERIC GENERATED ALWAYS AS (amount * 2) STORED,
    created_at TIMESTAMPTZ DEFAULT '2026-10-25 00:00:00+00',
    blob_data BYTEA,
    active BOOLEAN DEFAULT TRUE,
    CONSTRAINT orders_pk PRIMARY KEY (id),
    CONSTRAINT orders_fk FOREIGN KEY (customer_id) REFERENCES public.customers(id),
    CONSTRAINT orders_amount_check CHECK (amount >= 0)
);
CREATE INDEX orders_customer_idx ON "Sales / Data"."Order""Items" (customer_id);
INSERT INTO "Sales / Data"."Order""Items" (id, customer_id, amount, note, payload, blob_data) VALUES
    (1, 1, 12345678901234567890.1234567890, NULL, '{"x": [1, true]}', decode('00ff', 'hex')),
    (2, 1, 0, '', '[]', NULL),
    (9223372036854775807, 1, 1, 'last', NULL, NULL);
CREATE TABLE public.no_pk (label TEXT, payload JSON);
INSERT INTO public.no_pk VALUES ('first', '{}'), ('second', '[]');
CREATE TABLE public.empty_table (id INTEGER PRIMARY KEY);
CREATE TABLE public.zero_columns ();
INSERT INTO public.zero_columns DEFAULT VALUES;
INSERT INTO public.zero_columns DEFAULT VALUES;
CREATE VIEW public.order_view AS SELECT id, note FROM "Sales / Data"."Order""Items";
CREATE MATERIALIZED VIEW public.order_summary AS SELECT id FROM "Sales / Data"."Order""Items";
CREATE TABLE public.secret_data (value TEXT);
INSERT INTO public.secret_data VALUES ('not readable by the target role');
CREATE TABLE public.side_effects (value INTEGER);
CREATE FUNCTION public.try_write() RETURNS INTEGER LANGUAGE plpgsql SECURITY DEFINER AS
$$ BEGIN INSERT INTO public.side_effects VALUES (1); RETURN 1; END $$;
CREATE VIEW public.mutating_view AS SELECT public.try_write() AS value;
CREATE VIEW public.slow_view AS SELECT 1 AS value FROM pg_catalog.pg_sleep(5);
`)
	execFixture(t, db, `GRANT USAGE ON SCHEMA public, "Sales / Data" TO `+pgx.Identifier{role}.Sanitize())
	execFixture(t, db, `GRANT SELECT ON public.customers, "Sales / Data"."Order""Items", public.no_pk,
public.empty_table, public.zero_columns, public.order_view, public.order_summary, public.mutating_view,
public.slow_view TO `+pgx.Identifier{role}.Sanitize())
}

func execFixture(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("integration fixture SQL failed: %v", err)
	}
}

func assertCount(t *testing.T, db *sql.DB, query string, want int, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("row count = %d, want %d", count, want)
	}
}

type apiClient struct {
	baseURL string
	client  *http.Client
	csrf    string
}

func (f *fixture) anonymous() *apiClient {
	jar, _ := cookiejar.New(nil)
	return &apiClient{baseURL: f.server.URL, client: &http.Client{Jar: jar, Timeout: 15 * time.Second}}
}

func (f *fixture) login(t *testing.T, username, password string) *apiClient {
	t.Helper()
	client := f.anonymous()
	client.do(t, "POST", "/auth/login", map[string]string{"username": username, "password": password}, 200)
	var response struct {
		CSRF string `json:"csrf_token"`
	}
	decode(t, client.do(t, "GET", "/auth/csrf", nil, 200), &response)
	client.csrf = response.CSRF
	if client.csrf == "" {
		t.Fatal("login did not establish a usable session and CSRF token")
	}
	return client
}

func (c *apiClient) do(t *testing.T, method, path string, input any, status int, modify ...func(*http.Request)) []byte {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	r, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		r.Header.Set("X-CSRF-Token", c.csrf)
	}
	for _, change := range modify {
		change(r)
	}
	response, err := c.client.Do(r)
	if err != nil {
		t.Fatalf("integration HTTP request failed: %v", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d; response: %s", method, path, response.StatusCode, status, content)
	}
	for _, secret := range []string{adminPassword, memberPassword, targetPassword} {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatal("HTTP response exposed credentials")
		}
	}
	return content
}

func (c *apiClient) page(t *testing.T, path string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/html")
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("browser page request failed: %v", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET %s: status/content-type = %d/%q", path, response.StatusCode, response.Header.Get("Content-Type"))
	}
	return content
}

func decode(t *testing.T, body []byte, output any) {
	t.Helper()
	if err := json.Unmarshal(body, output); err != nil {
		t.Fatalf("invalid JSON response: %s", body)
	}
}

func registration(name string, settings database.ConnectionConfig) map[string]any {
	return map[string]any{"name": name, "dbms": settings.DBMS, "host": settings.Host, "port": settings.Port,
		"database_name": settings.DatabaseName, "username": settings.Username, "password": settings.Password, "ssl_mode": settings.SSLMode}
}

func tableURL(id, schema, table string) string {
	return fmt.Sprintf("/databases/%s/schemas/%s/tables/%s", id, url.PathEscape(schema), url.PathEscape(table))
}

// migrationFixture menghubungkan API dan repository nyata dengan provider uji
// approval serta antrean. Pelaksanaan job dipanggil langsung melalui hook migration.
type migrationFixture struct {
	*fixture
	definitions                        *migration.Repository
	migrations                         *migration.Service
	approvals                          *migrationTestApprovals
	editor, reader, platformAdmin      *apiClient
	databaseID, actorID, editorGroupID string
}

type migrationTestApproval struct {
	actorID string
	plan    migration.ActionPlan
}

type migrationTestApprovals struct {
	mu        sync.Mutex
	snapshots map[string]migrationTestApproval
	next      int
}

func (a *migrationTestApprovals) ValidateApproval(_ context.Context, request migration.ExecutionRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	approved, exists := a.snapshots[request.ApprovalID]
	if !exists || approved.actorID != request.ActorID || approved.plan != request.Plan {
		return migration.ErrApprovalRequired
	}
	return nil
}

// Antrean uji menyimpan payload di platform agar receipt dapat dibaca kembali.
type migrationTestQueue struct{ db *sql.DB }

func (q migrationTestQueue) Enqueue(ctx context.Context, request migration.ExecutionRequest) (string, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO integration_migration_jobs (id, payload) VALUES ($1, $2)`, request.ExecutionID, string(payload))
	return request.ExecutionID, err
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	definitions, err := migration.NewRepository(f.platform)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	execFixture(t, f.platform, `CREATE TABLE integration_migration_jobs (id UUID PRIMARY KEY, payload JSONB NOT NULL)`)
	execFixture(t, f.target, `GRANT CREATE ON SCHEMA public TO `+pgx.Identifier{f.createdRole}.Sanitize())
	approvals := &migrationTestApprovals{snapshots: make(map[string]migrationTestApproval)}
	authService := access.NewService(f.platform)
	migrations := migration.NewServiceWithExecution(f.openTarget, definitions, migration.ExecutionDependencies{
		Authorizer: authService, Approvals: approvals, Queue: migrationTestQueue{f.platform},
	})
	auth := access.NewHTTPHandler(authService, false)
	f.server.Close()
	f.server = httptest.NewServer(telemetry.HTTPHandler(app.RoutesWithModulesAndFrontend(auth,
		databasehttp.NewHandler(f.registry, f.pools, auth),
		databasehttp.NewViewerHandler(f.registry, f.inspector, auth),
		migration.NewHTTPHandler(migrations, auth), nil)))
	result := &migrationFixture{fixture: f, definitions: definitions, migrations: migrations, approvals: approvals}
	result.platformAdmin = f.login(t, "admin", adminPassword)
	var registered struct {
		Database database.Database `json:"database"`
	}
	decode(t, result.platformAdmin.do(t, "POST", "/admin/databases", registration("Migration target", f.settings), 201), &registered)
	result.databaseID = registered.Database.ID
	for _, permission := range []access.Permission{access.ViewEdit, access.ViewOnly} {
		var user struct {
			User access.User `json:"user"`
		}
		username := "migration_" + string(permission)
		decode(t, result.platformAdmin.do(t, "POST", "/admin/users", map[string]any{"username": username, "password": memberPassword}, 201), &user)
		var group struct {
			Group access.Group `json:"group"`
		}
		decode(t, result.platformAdmin.do(t, "POST", "/admin/groups", map[string]string{"name": username}, 201), &group)
		result.platformAdmin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/members/"+user.User.ID, nil, 204)
		result.platformAdmin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/databases/"+result.databaseID, map[string]any{"permission": permission}, 204)
		if permission == access.ViewEdit {
			result.actorID, result.editorGroupID = user.User.ID, group.Group.ID
			result.editor = f.login(t, username, memberPassword)
		} else {
			result.reader = f.login(t, username, memberPassword)
		}
	}
	return result
}

func (f *migrationFixture) migrationPath() string {
	return "/databases/" + f.databaseID + "/migrations"
}

func (f *migrationFixture) createMigration(t *testing.T, name, up, down string) migration.Migration {
	t.Helper()
	var result struct {
		Migration migration.Migration `json:"migration"`
	}
	decode(t, f.editor.do(t, "POST", f.migrationPath()+"/definitions", map[string]any{
		"name": name, "sql_up": up, "sql_down": down,
	}, 201), &result)
	return result.Migration
}

func (f *migrationFixture) queueAction(t *testing.T, definition migration.Migration, action migration.Action, modify ...func(*http.Request)) migration.ExecutionRequest {
	t.Helper()
	plan, err := f.migrations.PrepareAction(context.Background(), f.databaseID, definition.ID, action)
	if err != nil {
		t.Fatal(err)
	}
	f.approvals.mu.Lock()
	f.approvals.next++
	approvalID := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.approvals.next)
	f.approvals.snapshots[approvalID] = migrationTestApproval{actorID: f.actorID, plan: plan}
	f.approvals.mu.Unlock()
	var receipt migration.QueuedAction
	decode(t, f.editor.do(t, "POST", f.migrationPath()+"/definitions/"+definition.ID+"/actions", map[string]any{
		"action": action, "approval_id": approvalID,
	}, 202, modify...), &receipt)
	if receipt.JobID == "" || receipt.ExecutionID == "" {
		t.Fatal("pengajuan tidak menghasilkan identitas job dan execution")
	}
	var payload []byte
	if err := f.platform.QueryRow(`SELECT payload FROM integration_migration_jobs WHERE id=$1`, receipt.JobID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var request migration.ExecutionRequest
	decode(t, payload, &request)
	if request.ExecutionID != receipt.ExecutionID || request.Plan != plan || request.ActorID != f.actorID {
		t.Fatal("payload antrean berbeda dari snapshot pengajuan")
	}
	return request
}

// executeJob menguji kontrak transaksi milik migration dengan koneksi registry.
// Pengujian loop worker, retry, dan status job menjadi cakupan modul eksekusi.
func (f *migrationFixture) executeJob(request migration.ExecutionRequest) (migration.Execution, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.executeJobContext(ctx, request)
}

// executeJobContext menghubungkan hook transaksi dengan context attempt pengujian.
func (f *migrationFixture) executeJobContext(ctx context.Context, request migration.ExecutionRequest) (migration.Execution, error) {
	db, _, err := f.openTarget(ctx, request.Plan.DatabaseID)
	if err != nil {
		return migration.Execution{}, err
	}
	defer db.Close()
	connection, err := db.Conn(ctx)
	if err != nil {
		return migration.Execution{}, err
	}
	defer connection.Close()
	key, err := migration.MigrationLockKey(request.Plan.DatabaseID)
	if err != nil {
		return migration.Execution{}, err
	}
	acquire := func() (lockErr error) {
		lockCtx, activity := telemetry.StartActivity(ctx, "migration_lock_acquire", slog.String("database_id", request.Plan.DatabaseID))
		defer activity.End(&lockErr)
		_, lockErr = connection.ExecContext(lockCtx, `SELECT pg_advisory_lock($1)`, key)
		return lockErr
	}
	if err := acquire(); err != nil {
		return migration.Execution{}, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, _ = connection.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, key)
	}()
	tx, err := connection.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return migration.Execution{}, err
	}
	defer tx.Rollback()
	prepared, err := f.migrations.PrepareExecution(ctx, tx, request)
	if err != nil {
		return migration.Execution{}, err
	}
	if committed, exists := prepared.Committed(); exists {
		return committed, nil
	}
	started := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, prepared.Plan().SQL); err != nil {
		return migration.Execution{}, err
	}
	execution, err := prepared.Record(ctx, started)
	if err != nil {
		return migration.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return migration.Execution{}, err
	}
	return execution, nil
}

func (f *migrationFixture) runAction(t *testing.T, definition migration.Migration, action migration.Action) migration.Execution {
	t.Helper()
	execution, err := f.executeJob(f.queueAction(t, definition, action))
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func (f *migrationFixture) executionHistory(t *testing.T, hideRollback bool) migration.ExecutionHistory {
	t.Helper()
	var result migration.ExecutionHistory
	decode(t, f.reader.do(t, "GET", f.migrationPath()+"/history?hide_rollback="+fmt.Sprint(hideRollback), nil, 200), &result)
	return result
}
