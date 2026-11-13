package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const platformURL = "http://platform.test"

func newTestHandler(t *testing.T, database *fakeDatabase, secureCookie bool) *HTTPHandler {
	t.Helper()
	return NewHTTPHandler(newTestService(t, database, testNow), secureCookie)
}

func signedInDatabase(platformAdmin bool) *fakeDatabase {
	row := activeSession(testNow)
	row.platformAdmin = platformAdmin
	return newFakeDatabase().withSession(row)
}

func newRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, platformURL+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func authenticatedRequest(method, path, body string) *http.Request {
	request := newRequest(method, path, body)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testSessionToken})
	request.Header.Set("X-CSRF-Token", testCSRFToken)
	return request
}

func sessionCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	return nil
}

func errorMessage(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return body.Error
}

func TestRequireDatabaseAccess(t *testing.T) {
	tests := []struct {
		name          string
		permission    Permission
		platformAdmin bool
		minimum       Permission
		wantStatus    int
	}{
		{name: "view-only grant allows reading", permission: ViewOnly, minimum: ViewOnly, wantStatus: http.StatusNoContent},
		{name: "view-only grant denies editing", permission: ViewOnly, minimum: ViewEdit, wantStatus: http.StatusForbidden},
		{name: "view-edit grant allows editing", permission: ViewEdit, minimum: ViewEdit, wantStatus: http.StatusNoContent},
		{name: "missing grant denies reading", minimum: ViewOnly, wantStatus: http.StatusForbidden},
		{name: "platform admin without grant is denied", platformAdmin: true, minimum: ViewOnly, wantStatus: http.StatusForbidden},
		{name: "unknown minimum is rejected", permission: ViewEdit, minimum: "owner", wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, signedInDatabase(test.platformAdmin).withPermission(test.permission), false)
			var granted Permission
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				granted = DatabasePermissionFromContext(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			request := authenticatedRequest(http.MethodGet, "/databases/"+testDatabaseID+"/data", "")
			request.SetPathValue("databaseID", testDatabaseID)
			response := httptest.NewRecorder()

			handler.RequireDatabaseAccess(test.minimum, next).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if (granted != "") != (test.wantStatus == http.StatusNoContent) || (granted != "" && granted != test.permission) {
				t.Fatalf("permission passed to handler = %q", granted)
			}
		})
	}
}

func TestProtectedRequestGuards(t *testing.T) {
	type guard func(*HTTPHandler, http.Handler) http.Handler
	api := func(h *HTTPHandler, next http.Handler) http.Handler { return h.RequireAuthenticated(next) }
	admin := func(h *HTTPHandler, next http.Handler) http.Handler { return h.RequirePlatformAdmin(true, next) }
	adminPage := func(h *HTTPHandler, next http.Handler) http.Handler { return h.RequireBrowserPage(true, true, next) }
	tests := []struct {
		name         string
		guard        guard
		database     *fakeDatabase
		request      func() *http.Request
		wantStatus   int
		wantLocation string
	}{
		{
			name: "api without session", guard: api, database: newFakeDatabase(), wantStatus: http.StatusUnauthorized,
			request: func() *http.Request { return newRequest(http.MethodGet, "/", "") },
		},
		{
			name: "page without session redirects to login", guard: adminPage, database: newFakeDatabase(), wantStatus: http.StatusSeeOther, wantLocation: "/login",
			request: func() *http.Request { return newRequest(http.MethodGet, "/", "") },
		},
		{
			name: "unknown session", guard: api, database: newFakeDatabase().withoutSession(), wantStatus: http.StatusUnauthorized,
			request: func() *http.Request { return authenticatedRequest(http.MethodGet, "/", "") },
		},
		{
			name: "session lookup failure", guard: api, database: newFakeDatabase().failQuery(sessionLookup, errDatabaseDown), wantStatus: http.StatusInternalServerError,
			request: func() *http.Request { return authenticatedRequest(http.MethodGet, "/", "") },
		},
		{
			name: "non admin on admin api", guard: admin, database: signedInDatabase(false), wantStatus: http.StatusForbidden,
			request: func() *http.Request { return authenticatedRequest(http.MethodGet, "/", "") },
		},
		{
			name: "non admin on admin page", guard: adminPage, database: signedInDatabase(false), wantStatus: http.StatusSeeOther, wantLocation: "/",
			request: func() *http.Request { return authenticatedRequest(http.MethodGet, "/", "") },
		},
		{
			name: "state change without csrf token", guard: api, database: signedInDatabase(false), wantStatus: http.StatusForbidden,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodPost, "/", "{}")
				request.Header.Del("X-CSRF-Token")
				return request
			},
		},
		{
			name: "state change with wrong csrf token", guard: api, database: signedInDatabase(false), wantStatus: http.StatusForbidden,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodDelete, "/", "")
				request.Header.Set("X-CSRF-Token", "forged-token")
				return request
			},
		},
		{
			name: "cross-site state change", guard: api, database: signedInDatabase(false), wantStatus: http.StatusForbidden,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodPost, "/", "{}")
				request.Header.Set("Sec-Fetch-Site", "cross-site")
				return request
			},
		},
		{
			name: "foreign origin state change", guard: api, database: signedInDatabase(false), wantStatus: http.StatusForbidden,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodPost, "/", "{}")
				request.Header.Set("Origin", "http://attacker.test")
				return request
			},
		},
		{
			name: "form csrf token is accepted", guard: api, database: signedInDatabase(false), wantStatus: http.StatusNoContent,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodPost, "/", "csrf_token="+testCSRFToken)
				request.Header.Del("X-CSRF-Token")
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return request
			},
		},
		{
			name: "read does not need csrf token", guard: api, database: signedInDatabase(false), wantStatus: http.StatusNoContent,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodGet, "/", "")
				request.Header.Del("X-CSRF-Token")
				return request
			},
		},
		{
			name: "same origin admin change", guard: admin, database: signedInDatabase(true), wantStatus: http.StatusNoContent,
			request: func() *http.Request {
				request := authenticatedRequest(http.MethodPost, "/", "{}")
				request.Header.Set("Origin", platformURL)
				return request
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var principal User
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				principal = CurrentUser(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			test.guard(newTestHandler(t, test.database, false), next).ServeHTTP(response, test.request())

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if location := response.Header().Get("Location"); location != test.wantLocation {
				t.Fatalf("location = %q, want %q", location, test.wantLocation)
			}
			if (principal.ID != "") != (test.wantStatus == http.StatusNoContent) {
				t.Fatalf("handler principal = %+v", principal)
			}
		})
	}
}

func TestProtectedRequestClearsRejectedSessionCookie(t *testing.T) {
	response := httptest.NewRecorder()
	handler := newTestHandler(t, newFakeDatabase().withoutSession(), false)
	handler.RequireAuthenticated(http.NotFoundHandler()).ServeHTTP(response, authenticatedRequest(http.MethodGet, "/", ""))

	cookie := sessionCookie(t, response)
	if cookie == nil || cookie.Value != "" || cookie.MaxAge >= 0 {
		t.Fatalf("rejected session cookie was not cleared: %+v", cookie)
	}
}

func TestProtectedRequestDeliversRotatedIdentifier(t *testing.T) {
	for _, secure := range []bool{false, true} {
		row := activeSession(testNow)
		row.rotationRequired = true
		response := httptest.NewRecorder()
		handler := newTestHandler(t, newFakeDatabase().withSession(row), secure)
		handler.RequireAuthenticated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, authenticatedRequest(http.MethodGet, "/", ""))

		cookie := sessionCookie(t, response)
		if cookie == nil || cookie.Value == "" || cookie.Value == testSessionToken {
			t.Fatalf("rotated identifier was not delivered: %+v", cookie)
		}
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Secure != secure {
			t.Fatalf("cookie attributes = %+v, secure handler %t", cookie, secure)
		}
	}
}

func TestLoginHandler(t *testing.T) {
	validBody := `{"username":"reader","password":"` + testUserPassword + `"}`
	tests := []struct {
		name       string
		body       string
		database   func(*testing.T) *fakeDatabase
		prepare    func(*http.Request)
		wantStatus int
	}{
		{name: "malformed json", body: `{"username":`, wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"username":"reader","password":"x","remember":true}`, wantStatus: http.StatusBadRequest},
		{name: "multiple json values", body: validBody + validBody, wantStatus: http.StatusBadRequest},
		{name: "missing password", body: `{"username":"reader"}`, wantStatus: http.StatusBadRequest},
		{
			name: "cross-site login", body: validBody, wantStatus: http.StatusForbidden,
			prepare: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		},
		{
			name: "wrong credentials", body: validBody, wantStatus: http.StatusUnauthorized,
			database: func(*testing.T) *fakeDatabase { return newFakeDatabase().onQuery(loginLookup, loginColumns) },
		},
		{
			name: "login database failure", body: validBody, wantStatus: http.StatusInternalServerError,
			database: func(*testing.T) *fakeDatabase { return newFakeDatabase().failQuery(loginLookup, errDatabaseDown) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := newFakeDatabase()
			if test.database != nil {
				database = test.database(t)
			}
			request := newRequest(http.MethodPost, "/auth/login", test.body)
			if test.prepare != nil {
				test.prepare(request)
			}
			response := httptest.NewRecorder()
			newTestHandler(t, database, false).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if cookie := sessionCookie(t, response); cookie != nil {
				t.Fatal("failed login issued a session cookie")
			}
			if strings.Contains(response.Body.String(), "db.internal") {
				t.Fatal("internal error details were exposed")
			}
		})
	}
}

func TestLoginHandlerIssuesCookieOnly(t *testing.T) {
	database := newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, true))
	request := newRequest(http.MethodPost, "/auth/login", `{"username":"reader","password":"`+testUserPassword+`"}`)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "previous-session-token"})
	response := httptest.NewRecorder()
	newTestHandler(t, database, true).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	cookie := sessionCookie(t, response)
	if cookie == nil || cookie.Value == "" || cookie.Value == "previous-session-token" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected session cookie %+v", cookie)
	}
	body := response.Body.String()
	if strings.Contains(body, cookie.Value) || strings.Contains(body, testUserPassword) || !strings.Contains(body, testUserID) {
		t.Fatalf("login response leaked a secret or omitted the user: %s", body)
	}
	revoked := database.executed("DELETE FROM access_sessions WHERE token_hash = $1 OR previous_token_hash = $1")
	if len(revoked) != 1 || !bytes.Equal(revoked[0].args[0].([]byte), tokenHash("previous-session-token")) {
		t.Fatal("login did not replace the previous session identifier")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authentication response may be cached")
	}
}

func TestBrowserLoginRedirects(t *testing.T) {
	form := func(password string) *http.Request {
		request := newRequest(http.MethodPost, "/auth/login", "username=reader&password="+password)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "text/html")
		return request
	}
	tests := []struct {
		name     string
		request  *http.Request
		database *fakeDatabase
		want     string
	}{
		{name: "successful login opens workspace", request: form(testUserPassword), database: newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, true)), want: "/"},
		{name: "failed login returns to form", request: form("wrong-password-1"), database: newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, true)), want: "/login?notice=login-error"},
		{name: "empty form returns to form", request: form(""), database: newFakeDatabase(), want: "/login?notice=login-error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			newTestHandler(t, test.database, false).ServeHTTP(response, test.request)
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != test.want {
				t.Fatalf("redirect = %d %q, want %q", response.Code, response.Header().Get("Location"), test.want)
			}
			if strings.Contains(response.Header().Get("Location"), testUserPassword) {
				t.Fatal("password appeared in the redirect URL")
			}
		})
	}
}

func TestLogoutHandler(t *testing.T) {
	database := signedInDatabase(false)
	response := httptest.NewRecorder()
	newTestHandler(t, database, false).ServeHTTP(response, authenticatedRequest(http.MethodPost, "/auth/logout", ""))

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if cookie := sessionCookie(t, response); cookie == nil || cookie.MaxAge >= 0 {
		t.Fatal("logout did not clear the session cookie")
	}
	if !database.ran("DELETE FROM access_sessions WHERE token_hash = $1 OR previous_token_hash = $1") {
		t.Fatal("logout did not revoke the backend session")
	}
}

func TestCurrentUserAndCSRFEndpoints(t *testing.T) {
	handler := newTestHandler(t, signedInDatabase(false), false)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/auth/csrf", ""))
	var csrf struct {
		Token string `json:"csrf_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&csrf); err != nil || csrf.Token != testCSRFToken {
		t.Fatalf("csrf endpoint = %d %q", response.Code, csrf.Token)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/auth/me", ""))
	if body := response.Body.String(); response.Code != http.StatusOK || !strings.Contains(body, testUsername) || strings.Contains(body, testSessionToken) {
		t.Fatalf("current user endpoint = %d %s", response.Code, body)
	}
}

func TestDatabaseAccessEndpoint(t *testing.T) {
	tests := []struct {
		permission Permission
		wantStatus int
	}{
		{permission: ViewEdit, wantStatus: http.StatusOK},
		{wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler := newTestHandler(t, signedInDatabase(false).withPermission(test.permission), false)
		handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/databases/"+testDatabaseID+"/access", ""))
		if response.Code != test.wantStatus {
			t.Fatalf("permission %q: status = %d, want %d", test.permission, response.Code, test.wantStatus)
		}
		if test.permission != "" && !strings.Contains(response.Body.String(), string(test.permission)) {
			t.Fatalf("effective permission missing from %s", response.Body.String())
		}
	}
}

func TestAdminHandlers(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		database    *fakeDatabase
		wantStatus  int
		wantMessage string
	}{
		{
			name: "invalid username explains the rule", method: http.MethodPost, path: "/admin/users", body: `{"username":"x","password":"` + validNewPassword + `"}`,
			database: signedInDatabase(true), wantStatus: http.StatusBadRequest, wantMessage: "username must contain 3 to 64 characters",
		},
		{
			name: "duplicate username", method: http.MethodPost, path: "/admin/users", body: `{"username":"reader","password":"` + validNewPassword + `"}`,
			database: signedInDatabase(true).failQuery(userInsert, sqlStateError("23505")), wantStatus: http.StatusConflict, wantMessage: "username already exists",
		},
		{
			name: "user storage failure stays internal", method: http.MethodPost, path: "/admin/users", body: `{"username":"reader","password":"` + validNewPassword + `"}`,
			database: signedInDatabase(true).failQuery(userInsert, errDatabaseDown), wantStatus: http.StatusInternalServerError, wantMessage: "internal error",
		},
		{
			name: "invalid group name explains the rule", method: http.MethodPost, path: "/admin/groups", body: `{"name":" "}`,
			database: signedInDatabase(true), wantStatus: http.StatusBadRequest, wantMessage: "group name must contain 1 to 100 characters",
		},
		{
			name: "group storage failure stays internal", method: http.MethodPost, path: "/admin/groups", body: `{"name":"Data Team"}`,
			database: signedInDatabase(true).failQuery(groupInsert, errDatabaseDown), wantStatus: http.StatusInternalServerError, wantMessage: "internal error",
		},
		{
			name: "admin cannot demote itself", method: http.MethodPatch, path: "/admin/users/" + testUserID, body: `{"active":true,"platform_admin":false}`,
			database: signedInDatabase(true), wantStatus: http.StatusConflict, wantMessage: "cannot disable or demote the current admin",
		},
		{
			name: "user update requires both flags", method: http.MethodPatch, path: "/admin/users/" + unknownID, body: `{"active":true}`,
			database: signedInDatabase(true), wantStatus: http.StatusBadRequest, wantMessage: "active and platform_admin are required",
		},
		{
			name: "removing a non member", method: http.MethodDelete, path: "/admin/groups/" + testGroupID + "/members/" + unknownID,
			database: signedInDatabase(true).onExec(memberDelete, 0), wantStatus: http.StatusNotFound, wantMessage: "not found",
		},
		{
			name: "unknown permission level", method: http.MethodPut, path: "/admin/groups/" + testGroupID + "/databases/" + testDatabaseID, body: `{"permission":"owner"}`,
			database: signedInDatabase(true), wantStatus: http.StatusBadRequest, wantMessage: "permission must be view_only or view_edit",
		},
		{
			name: "non admin cannot manage users", method: http.MethodPost, path: "/admin/users", body: `{"username":"reader","password":"` + validNewPassword + `"}`,
			database: signedInDatabase(false), wantStatus: http.StatusForbidden, wantMessage: "forbidden",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			newTestHandler(t, test.database, false).ServeHTTP(response, authenticatedRequest(test.method, test.path, test.body))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if message := errorMessage(t, response); message != test.wantMessage {
				t.Fatalf("message = %q, want %q", message, test.wantMessage)
			}
		})
	}
}

func TestWriteServiceError(t *testing.T) {
	tests := []struct {
		err         error
		wantStatus  int
		wantMessage string
	}{
		{err: ErrNotFound, wantStatus: http.StatusNotFound, wantMessage: "not found"},
		{err: ErrConflict, wantStatus: http.StatusConflict, wantMessage: "conflict"},
		{err: ErrForbidden, wantStatus: http.StatusForbidden, wantMessage: "forbidden"},
		{err: ErrInvalidInput, wantStatus: http.StatusBadRequest, wantMessage: "invalid request"},
		{err: invalidInput("name is required"), wantStatus: http.StatusBadRequest, wantMessage: "name is required"},
		{err: errDatabaseDown, wantStatus: http.StatusInternalServerError, wantMessage: "internal error"},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		writeServiceError(response, t.Context(), test.err)
		if response.Code != test.wantStatus || errorMessage(t, response) != test.wantMessage {
			t.Errorf("%v mapped to %d", test.err, response.Code)
		}
	}
}

func TestValidCSRF(t *testing.T) {
	tests := []struct {
		provided, expected string
		want               bool
	}{
		{provided: testCSRFToken, expected: testCSRFToken, want: true},
		{provided: "", expected: ""},
		{provided: "", expected: testCSRFToken},
		{provided: testCSRFToken + "x", expected: testCSRFToken},
		{provided: strings.ToUpper(testCSRFToken), expected: testCSRFToken},
	}
	for _, test := range tests {
		if got := validCSRF(test.provided, test.expected); got != test.want {
			t.Errorf("validCSRF(%q, %q) = %t", test.provided, test.expected, got)
		}
	}
}

func TestSecureOriginRequiresHTTPS(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{origin: "", want: true},
		{origin: "https://platform.test", want: true},
		{origin: "http://platform.test"},
		{origin: "https://attacker.test"},
		{origin: "null"},
	}
	handler := &HTTPHandler{secureCookie: true}
	for _, test := range tests {
		request := newRequest(http.MethodPost, "/", "")
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
		}
		if got := handler.validRequestOrigin(request); got != test.want {
			t.Errorf("origin %q accepted = %t, want %t", test.origin, got, test.want)
		}
	}
}

func TestAccessAuditUsesVerifiedActorAndProtectsSession(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&output, "server"))
	t.Cleanup(func() { slog.SetDefault(previous) })
	handler := newTestHandler(t, signedInDatabase(false).withPermission(ViewOnly), false)
	protected := handler.RequireDatabaseAccess(ViewEdit, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("edit access must be denied")
	}))
	request := authenticatedRequest(http.MethodGet, "/databases/"+testDatabaseID+"/data", "")
	request.SetPathValue("databaseID", testDatabaseID)
	response := httptest.NewRecorder()
	telemetry.HTTPHandler(protected).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
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
				t.Fatalf("audit record does not identify the verified actor: %v", record)
			}
		}
	}
	if !found {
		t.Fatal("access denial was not audited")
	}
	if strings.Contains(raw, testSessionToken) || strings.Contains(raw, testCSRFToken) {
		t.Fatal("session secrets appeared in the audit log")
	}
}
