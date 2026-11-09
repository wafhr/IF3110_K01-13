package access_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
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

func TestAccessAuditUsesVerifiedActorAndProtectsSession(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&output, "server"))
	t.Cleanup(func() { slog.SetDefault(previous) })
	handler := newHandler(t, access.ViewOnly, false)
	protected := handler.RequireDatabaseAccess(access.ViewEdit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("izin edit seharusnya ditolak") }))
	request := httptest.NewRequest(http.MethodGet, "/databases/database/data", nil)
	request.SetPathValue("databaseID", "00000000-0000-4000-8000-000000000002")
	request.AddCookie(&http.Cookie{Name: "platform_session", Value: testSessionToken})
	response := httptest.NewRecorder()
	telemetry.HTTPHandler(protected).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatal("respons penolakan akses berubah")
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
		if record["event"] == "access_denied" {
			found = true
			if record["actor"] != testUserID || record["reason"] != "database_permission_required" || record["status"] != "failed" {
				t.Fatal("audit penolakan tidak sesuai identitas terverifikasi")
			}
		}
	}
	if !found {
		t.Fatal("penolakan akses tidak dicatat")
	}
	if strings.Contains(raw, testSessionToken) || strings.Contains(raw, "test-csrf") || strings.Contains(raw, `"reader"`) {
		t.Fatal("credential atau session muncul pada audit")
	}
}
