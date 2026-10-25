package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/app"
)

func TestRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", http.StatusOK},
		{"POST", "/health", http.StatusMethodNotAllowed},
		{"GET", "/missing", http.StatusNotFound},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			app.Routes().ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.status == http.StatusOK {
				if response.Body.String() != "{\"status\":\"ok\"}\n" {
					t.Fatalf("unexpected health response: %s", response.Body.String())
				}
				if response.Header().Get("Content-Type") != "application/json" {
					t.Fatal("expected JSON content type")
				}
			}
		})
	}
}

func TestModuleRouting(t *testing.T) {
	module := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(name))
		})
	}
	handler := app.RoutesWithModules(module("access"), module("database"), module("viewer"), module("migration"))
	for _, tc := range []struct{ method, path, module string }{
		{"POST", "/auth/login", "access"},
		{"GET", "/admin/groups", "access"},
		{"GET", "/admin/databases", "database"},
		{"POST", "/admin/databases", "database"},
		{"PUT", "/admin/databases/id", "database"},
		{"POST", "/admin/databases/id/test-connection", "database"},
		{"GET", "/databases", "viewer"},
		{"GET", "/databases/id/access", "access"},
		{"GET", "/databases/id/schemas", "viewer"},
		{"GET", "/databases/id/schemas/migrations/tables", "viewer"},
		{"GET", "/databases/id/schemas/public/tables/migrations/data", "viewer"},
		{"GET", "/databases/id/migrations", "migration"},
		{"POST", "/databases/id/migrations", "migration"},
		{"GET", "/databases/id/migrations/1", "migration"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != http.StatusOK || response.Body.String() != tc.module {
				t.Fatalf("status/body = %d/%q, want 200/%q", response.Code, response.Body.String(), tc.module)
			}
		})
	}

	// Existing migration-only callers retain their registration route.
	legacy := app.RoutesWithMigration(module("access"), module("migration"))
	response := httptest.NewRecorder()
	legacy.ServeHTTP(response, httptest.NewRequest("POST", "/admin/databases", nil))
	if response.Body.String() != "migration" {
		t.Fatalf("migration-only registration routed to %q", response.Body.String())
	}
}

func TestCombinedModuleAndFrontendRouting(t *testing.T) {
	module := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(name))
		})
	}
	handler := app.RoutesWithModulesAndFrontend(
		module("access"), module("database"), module("viewer"), module("migration"), module("frontend"),
	)
	for _, tc := range []struct{ path, want string }{
		{"/login", "frontend"},
		{"/admin/databases", "database"},
		{"/databases/id/access", "access"},
		{"/databases/id/schemas", "viewer"},
		{"/databases/id/migrations", "migration"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusOK || response.Body.String() != tc.want {
				t.Fatalf("status/body = %d/%q, want 200/%q", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}
