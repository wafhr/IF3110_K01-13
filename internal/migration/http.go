package migration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

type HTTPHandler struct {
	service *Service
	routes  *http.ServeMux
}

func NewHTTPHandler(service *Service, auth *access.HTTPHandler) *HTTPHandler {
	h := &HTTPHandler{service: service, routes: http.NewServeMux()}
	h.routes.Handle("GET /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.list)))
	h.routes.Handle("POST /databases/{databaseID}/migrations", auth.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(h.execute)))
	h.routes.Handle("GET /databases/{databaseID}/migrations/{version}", auth.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(h.detail)))
	h.routes.Handle("POST /admin/databases", auth.RequirePlatformAdmin(true, http.HandlerFunc(h.registerDatabase)))
	return h
}
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.routes.ServeHTTP(w, r) }

func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.service.List(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		migrationError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "html" || r.Header.Get("Accept") == "text/html" {
		renderEditor(w, r, rows, access.DatabasePermissionFromContext(r.Context()) == access.ViewEdit)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migrations": rows})
}
func (h *HTTPHandler) detail(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || v < 1 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid version"})
		return
	}
	row, err := h.service.Get(r.Context(), r.PathValue("databaseID"), v)
	if err != nil {
		migrationError(w, err)
		return
	}
	if r.URL.Query().Get("format") == "html" || r.Header.Get("Accept") == "text/html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = detailTemplate.Execute(w, struct {
			Record
			DatabaseID string
		}{Record: row, DatabaseID: r.PathValue("databaseID")})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"migration": row})
}
func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		SQL  string `json:"sql"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}
	user := access.CurrentUser(r.Context())
	row, err := h.service.Execute(r.Context(), r.PathValue("databaseID"), user.ID, input.Name, input.SQL)
	if err != nil {
		if row.Version > 0 {
			jsonResponse(w, http.StatusUnprocessableEntity, map[string]any{"migration": row, "error": "migration failed and was rolled back"})
			return
		}
		migrationError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"migration": row})
}
func (h *HTTPHandler) registerDatabase(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name          string `json:"name"`
		ConnectionURL string `json:"connection_url"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return
	}
	id, err := h.service.RegisterDatabase(r.Context(), input.Name, input.ConnectionURL)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"database_id": id})
}

var editorTemplate = template.Must(template.New("migration").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Database migrations</title><style>body{font:15px system-ui;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#18212b}textarea{width:100%;height:18rem;font:14px monospace}input{padding:.5rem;width:60%}button{padding:.6rem 1rem}table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid #ddd;padding:.5rem;text-align:left}pre{white-space:pre-wrap}</style></head><body><h1>Migrations</h1>{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}<h2>History</h2><table><tr><th>Version</th><th>Name</th><th>Status</th><th>Executed by</th><th>Started</th></tr>{{range .Rows}}<tr><td><a href="/databases/{{$.DatabaseID}}/migrations/{{.Version}}">{{.Version}}</a></td><td>{{.Name}}</td><td>{{.Status}}</td><td>{{.ExecutedBy}}</td><td>{{.StartedAt}}</td></tr>{{end}}</table>{{if .CanEdit}}<h2>New migration</h2><p>Submit JSON to POST this endpoint with the session CSRF token in X-CSRF-Token.</p><label>Name <input id="name" maxlength="160"></label><textarea id="sql" placeholder="-- Write migration SQL"></textarea><button id="run">Run migration</button><pre id="result"></pre><script>document.getElementById('run').onclick=async()=>{const c=await fetch('/auth/csrf');const t=(await c.json()).csrf_token;const r=await fetch(location.pathname,{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':t},body:JSON.stringify({name:document.getElementById('name').value,sql:document.getElementById('sql').value})});document.getElementById('result').textContent=await r.text();if(r.ok)location.reload()};</script>{{end}}</body></html>`))

var detailTemplate = template.Must(template.New("migration-detail").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Migration {{.Version}}</title><style>body{font:15px system-ui;max-width:1000px;margin:2rem auto;padding:0 1rem;color:#18212b}pre{white-space:pre-wrap;background:#f4f6f8;padding:1rem;overflow:auto}</style></head><body><p><a href="/databases/{{.DatabaseID}}/migrations?format=html">← Migration history</a></p><h1>Migration {{.Version}}: {{.Name}}</h1><p>Status: {{.Status}} · Executed by {{.ExecutedBy}} · {{.StartedAt}}</p><h2>SQL</h2><pre>{{.SQL}}</pre></body></html>`))

func renderEditor(w http.ResponseWriter, r *http.Request, rows []Record, canEdit bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := editorTemplate.Execute(w, struct {
		Rows       []Record
		DatabaseID string
		CanEdit    bool
		Error      string
	}{Rows: rows, DatabaseID: r.PathValue("databaseID"), CanEdit: canEdit, Error: ""}); err != nil {
		slog.Error("render migration page failed")
	}
}
func migrationError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "database or migration not found"})
		return
	}
	slog.Error("migration operation failed", "error", err)
	jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "migration operation failed"})
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
