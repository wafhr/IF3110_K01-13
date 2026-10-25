package access_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

func TestRequireDatabaseAccess(t *testing.T) {
	tests := []struct {
		name           string
		permission     access.Permission
		platformAdmin  bool
		minimum        access.Permission
		wantStatusCode int
		wantNext       bool
	}{
		{
			name:           "view-only grant allows reading",
			permission:     access.ViewOnly,
			minimum:        access.ViewOnly,
			wantStatusCode: http.StatusNoContent,
			wantNext:       true,
		},
		{
			name:           "view-only grant denies editing",
			permission:     access.ViewOnly,
			minimum:        access.ViewEdit,
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "view-edit grant allows editing",
			permission:     access.ViewEdit,
			minimum:        access.ViewEdit,
			wantStatusCode: http.StatusNoContent,
			wantNext:       true,
		},
		{
			name:           "missing grant denies reading",
			minimum:        access.ViewOnly,
			wantStatusCode: http.StatusForbidden,
		},
		{
			name:           "platform admin without grant is denied",
			platformAdmin:  true,
			minimum:        access.ViewOnly,
			wantStatusCode: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newHandler(t, test.permission, test.platformAdmin)
			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodGet, "/databases/00000000-0000-4000-8000-000000000002/data", nil)
			request.SetPathValue("databaseID", "00000000-0000-4000-8000-000000000002")
			request.AddCookie(&http.Cookie{Name: "platform_session", Value: "test-session-token"})
			response := httptest.NewRecorder()

			handler.RequireDatabaseAccess(test.minimum, next).ServeHTTP(response, request)

			if response.Code != test.wantStatusCode {
				t.Errorf("status = %d, want %d", response.Code, test.wantStatusCode)
			}
			if nextCalled != test.wantNext {
				t.Errorf("next handler called = %t, want %t", nextCalled, test.wantNext)
			}
		})
	}
}

func TestRequireDatabaseAccessRejectsMissingSession(t *testing.T) {
	handler := newHandler(t, access.ViewEdit, false)
	request := httptest.NewRequest(http.MethodGet, "/databases/00000000-0000-4000-8000-000000000002/data", nil)
	request.SetPathValue("databaseID", "00000000-0000-4000-8000-000000000002")
	response := httptest.NewRecorder()

	handler.RequireDatabaseAccess(access.ViewOnly, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next handler must not run without a session")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func newHandler(t *testing.T, permission access.Permission, platformAdmin bool) *access.HTTPHandler {
	t.Helper()
	db := sql.OpenDB(newStubConnector(string(permission), platformAdmin))
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close stub database: %v", err)
		}
	})
	return access.NewHTTPHandler(access.NewService(db), false)
}
