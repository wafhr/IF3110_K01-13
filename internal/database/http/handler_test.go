package databasehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

const testDatabaseID = "30000000-0000-4000-8000-000000000001"

type fakeRegistry struct {
	created  database.ConnectionConfig
	name     string
	err      error
	deleted  string
	listNil  bool
	accessed string
}

func (f *fakeRegistry) Create(_ context.Context, name string, config database.ConnectionConfig) (database.Database, error) {
	f.name, f.created = name, config
	if f.err != nil {
		return database.Database{}, f.err
	}
	return database.Database{ID: testDatabaseID, Name: name, DBMS: config.DBMS, Host: config.Host}, nil
}

func (f *fakeRegistry) Get(_ context.Context, id string) (database.Database, error) {
	if f.err != nil {
		return database.Database{}, f.err
	}
	return database.Database{ID: id, Name: "orders"}, nil
}

func (f *fakeRegistry) List(context.Context) ([]database.Database, error) {
	if f.listNil {
		return nil, f.err
	}
	return []database.Database{{ID: testDatabaseID, Name: "orders"}}, f.err
}

func (f *fakeRegistry) Update(_ context.Context, id, name string, config database.ConnectionConfig) (database.Database, error) {
	f.name, f.created = name, config
	return database.Database{ID: id, Name: name}, f.err
}

func (f *fakeRegistry) DeleteWithAccessCleanup(_ context.Context, id string) error {
	f.deleted = id
	return f.err
}

func (f *fakeRegistry) ListAccessible(_ context.Context, userID string) ([]database.AccessibleDatabase, error) {
	f.accessed = userID
	return nil, f.err
}

type fakeConnections struct {
	tested      database.ConnectionConfig
	checked     string
	invalidated string
	err         error
}

func (f *fakeConnections) Check(_ context.Context, id string) error {
	f.checked = id
	return f.err
}

func (f *fakeConnections) Test(_ context.Context, config database.ConnectionConfig) error {
	f.tested = config
	return f.err
}

func (f *fakeConnections) Invalidate(id string) error {
	f.invalidated = id
	return nil
}

// managementRoutes mirrors NewHandler without the access middleware, which the
// access package and integration tests cover.
func managementRoutes(h *Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/databases", h.list)
	mux.HandleFunc("POST /admin/databases", h.create)
	mux.HandleFunc("POST /admin/databases/test-connection", h.testConnection)
	mux.HandleFunc("GET /admin/databases/{databaseID}", h.get)
	mux.HandleFunc("PUT /admin/databases/{databaseID}", h.update)
	mux.HandleFunc("DELETE /admin/databases/{databaseID}", h.delete)
	mux.HandleFunc("POST /admin/databases/{databaseID}/test-connection", h.checkConnection)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveAPI(w, r, mux) })
}

func call(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

const validConnection = `{"dbms":"postgres","host":"db.internal","port":5432,"database_name":"orders","username":"reader","password":"secret-value","ssl_mode":"require"}`

func TestCreateDatabaseNormalizesAndHidesPassword(t *testing.T) {
	registry := &fakeRegistry{}
	handler := managementRoutes(&Handler{registry: registry, connections: &fakeConnections{}})
	body := `{"name":"Orders",` + strings.TrimPrefix(validConnection, "{")
	response := call(t, handler, "POST", "/admin/databases", body)
	if response.Code != http.StatusCreated || response.Header().Get("Location") != "/admin/databases/"+testDatabaseID {
		t.Fatalf("status = %d, location = %q, body = %s", response.Code, response.Header().Get("Location"), response.Body)
	}
	if registry.name != "Orders" || registry.created.Password != "secret-value" || registry.created.Port != 5432 {
		t.Fatalf("registry received %q %+v", registry.name, registry.created)
	}
	if strings.Contains(response.Body.String(), "secret-value") {
		t.Fatal("response exposed the connection password")
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("API response is cacheable or sniffable")
	}
}

func TestDatabaseRequestValidation(t *testing.T) {
	handler := managementRoutes(&Handler{registry: &fakeRegistry{}, connections: &fakeConnections{}})
	tests := []struct {
		name, body, contentType string
		want                    int
	}{
		{name: "missing password", body: `{"name":"Orders","dbms":"postgres","host":"db","port":5432,"database_name":"orders","username":"reader","ssl_mode":"require"}`, want: http.StatusBadRequest},
		{name: "port out of range", body: `{"name":"Orders","dbms":"postgres","host":"db","port":70000,"database_name":"orders","username":"reader","password":"x","ssl_mode":"require"}`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"name":"Orders","extra":true}`, want: http.StatusBadRequest},
		{name: "two JSON values", body: `{"name":"Orders"} {}`, want: http.StatusBadRequest},
		{name: "wrong content type", body: `{}`, contentType: "text/plain", want: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/admin/databases", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, test.want, response.Body)
			}
		})
	}
	large := `{"name":"` + strings.Repeat("a", 1<<20) + `"}`
	if response := call(t, handler, "POST", "/admin/databases", large); response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body status = %d", response.Code)
	}
}

func TestConnectionTestsAndCacheInvalidation(t *testing.T) {
	registry, connections := &fakeRegistry{}, &fakeConnections{}
	handler := managementRoutes(&Handler{registry: registry, connections: connections})
	if response := call(t, handler, "POST", "/admin/databases/test-connection", validConnection); response.Code != http.StatusOK || connections.tested.Host != "db.internal" {
		t.Fatalf("unsaved test status = %d, tested %+v", response.Code, connections.tested)
	}
	if response := call(t, handler, "POST", "/admin/databases/"+testDatabaseID+"/test-connection", ""); response.Code != http.StatusOK || connections.checked != testDatabaseID {
		t.Fatalf("saved test status = %d, checked %q", response.Code, connections.checked)
	}
	body := `{"name":"Orders",` + strings.TrimPrefix(validConnection, "{")
	if response := call(t, handler, "PUT", "/admin/databases/"+testDatabaseID, body); response.Code != http.StatusOK || connections.invalidated != testDatabaseID {
		t.Fatalf("update status = %d, invalidated %q", response.Code, connections.invalidated)
	}
	connections.invalidated = ""
	if response := call(t, handler, "DELETE", "/admin/databases/"+testDatabaseID, ""); response.Code != http.StatusNoContent || registry.deleted != testDatabaseID || connections.invalidated != testDatabaseID {
		t.Fatalf("delete status = %d, deleted %q, invalidated %q", response.Code, registry.deleted, connections.invalidated)
	}
}

func TestListAndGetDatabases(t *testing.T) {
	handler := managementRoutes(&Handler{registry: &fakeRegistry{listNil: true}, connections: &fakeConnections{}})
	response := call(t, handler, "GET", "/admin/databases", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"databases":[]`) {
		t.Fatalf("empty list = %d %s", response.Code, response.Body)
	}
	handler = managementRoutes(&Handler{registry: &fakeRegistry{}, connections: &fakeConnections{}})
	if response := call(t, handler, "GET", "/admin/databases/"+testDatabaseID, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), testDatabaseID) {
		t.Fatalf("get = %d %s", response.Code, response.Body)
	}
}

func TestServiceErrorsMapToSafeResponses(t *testing.T) {
	tests := []struct {
		err     error
		status  int
		message string
	}{
		{database.ErrInvalidInput, http.StatusBadRequest, "invalid database configuration"},
		{database.ErrNotFound, http.StatusNotFound, "database not found"},
		{database.ErrObjectNotFound, http.StatusNotFound, "schema or table not found"},
		{database.ErrConflict, http.StatusConflict, "database name already exists"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "database operation timed out"},
		{context.Canceled, http.StatusRequestTimeout, "request canceled"},
		{targetdb.ErrAuthentication, http.StatusBadGateway, "target database authentication failed"},
		{targetdb.ErrPermissionDenied, http.StatusBadGateway, "target database access denied"},
		{targetdb.ErrConnection, http.StatusBadGateway, "target database connection failed"},
		{targetdb.ErrClosed, http.StatusServiceUnavailable, "database connections unavailable"},
		{errors.New("password=secret host=10.0.0.5"), http.StatusInternalServerError, "internal error"},
	}
	for _, test := range tests {
		connections := &fakeConnections{err: test.err}
		handler := managementRoutes(&Handler{registry: &fakeRegistry{}, connections: connections})
		response := call(t, handler, "POST", "/admin/databases/"+testDatabaseID+"/test-connection", "")
		var body map[string]string
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.Code != test.status || body["error"] != test.message {
			t.Errorf("%v: status = %d, error = %q", test.err, response.Code, body["error"])
		}
	}
}

type fakeSchemas struct {
	options database.PageOptions
	err     error
}

func (f *fakeSchemas) Schemas(context.Context, string) ([]database.Schema, error) { return nil, f.err }
func (f *fakeSchemas) Tables(context.Context, string, string) ([]database.Table, error) {
	return []database.Table{{Schema: "public", Name: "orders", Type: "table"}}, f.err
}
func (f *fakeSchemas) Table(_ context.Context, _, schema, name string) (database.TableDetails, error) {
	return database.TableDetails{Table: database.Table{Schema: schema, Name: name, Type: "table"}}, f.err
}
func (f *fakeSchemas) Data(_ context.Context, _, _, _ string, options database.PageOptions) (database.DataPage, error) {
	f.options = options
	return database.DataPage{}, f.err
}

func viewerRoutes(h *ViewerHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /databases/{databaseID}/schemas", h.listSchemas)
	mux.HandleFunc("GET /databases/{databaseID}/schemas/{schema}/tables", h.listTables)
	mux.HandleFunc("GET /databases/{databaseID}/schemas/{schema}/tables/{table}", h.tableDetails)
	mux.HandleFunc("GET /databases/{databaseID}/schemas/{schema}/tables/{table}/data", h.tableData)
	mux.HandleFunc("GET /databases", h.listAccessible)
	return mux
}

func TestViewerReadsSchemasTablesAndPages(t *testing.T) {
	schemas := &fakeSchemas{}
	handler := viewerRoutes(&ViewerHandler{registry: &fakeRegistry{}, schemas: schemas})
	base := "/databases/" + testDatabaseID + "/schemas"
	if response := call(t, handler, "GET", base, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"schemas":[]`) {
		t.Fatalf("schemas = %d %s", response.Code, response.Body)
	}
	if response := call(t, handler, "GET", base+"/public/tables", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "orders") {
		t.Fatalf("tables = %d %s", response.Code, response.Body)
	}
	if response := call(t, handler, "GET", base+"/public/tables/orders", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"orders"`) {
		t.Fatalf("table = %d %s", response.Code, response.Body)
	}
	if response := call(t, handler, "GET", base+"/public/tables/orders/data?limit=25&offset=50", ""); response.Code != http.StatusOK || schemas.options.Limit != 25 || schemas.options.Offset != 50 {
		t.Fatalf("data = %d, options = %+v", response.Code, schemas.options)
	}
	if response := call(t, handler, "GET", "/databases", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("accessible list without principal = %d", response.Code)
	}
}

func TestViewerRejectsInvalidRequestsAndHidesErrors(t *testing.T) {
	base := "/databases/" + testDatabaseID + "/schemas/public/tables/orders"
	handler := viewerRoutes(&ViewerHandler{registry: &fakeRegistry{}, schemas: &fakeSchemas{}})
	for _, query := range []string{"limit=0", "limit=abc", "offset=-1", "page=2", "limit=1&limit=2", "limit="} {
		if response := call(t, handler, "GET", base+"/data?"+query, ""); response.Code != http.StatusBadRequest {
			t.Errorf("query %q status = %d", query, response.Code)
		}
	}
	tests := map[error]int{
		targetdb.ErrPermissionDenied: http.StatusForbidden,
		database.ErrInvalidInput:     http.StatusBadRequest,
		database.ErrObjectNotFound:   http.StatusNotFound,
		targetdb.ErrConnection:       http.StatusBadGateway,
	}
	for err, status := range tests {
		handler := viewerRoutes(&ViewerHandler{registry: &fakeRegistry{}, schemas: &fakeSchemas{err: err}})
		if response := call(t, handler, "GET", base, ""); response.Code != status {
			t.Errorf("%v status = %d, want %d", err, response.Code, status)
		}
	}
}
