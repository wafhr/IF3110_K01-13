//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

const sessionCookieName = "platform_session"

type rawResponse struct {
	status int
	cookie *http.Cookie
	body   []byte
}

func TestAccessControlAuthenticationIntegration(t *testing.T) {
	f := newFixture(t)
	admin := f.login(t, "admin", adminPassword)
	createAccessUser(t, admin, "analyst", false)

	t.Run("passwords are stored as salted hashes", func(t *testing.T) {
		var stored string
		if err := f.platform.QueryRow(`SELECT password_hash FROM access_users WHERE username = 'analyst'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(stored, "pbkdf2-sha256$") || strings.Contains(stored, memberPassword) {
			t.Fatal("password was not stored as a PBKDF2 hash")
		}
	})

	t.Run("invalid credentials do not create a session", func(t *testing.T) {
		for _, credentials := range []map[string]string{
			{"username": "analyst", "password": "not-the-right-password"},
			{"username": "nobody", "password": memberPassword},
		} {
			response := f.send(t, http.MethodPost, "/auth/login", "", credentials)
			if response.status != http.StatusUnauthorized || response.cookie != nil {
				t.Fatalf("login for %q: status %d, cookie issued %t", credentials["username"], response.status, response.cookie != nil)
			}
		}
		if response := f.send(t, http.MethodPost, "/auth/login", "", map[string]string{"username": "analyst"}); response.status != http.StatusBadRequest {
			t.Fatalf("incomplete login status = %d", response.status)
		}
		assertCount(t, f.platform, `SELECT count(*) FROM access_sessions s JOIN access_users u ON u.id = s.user_id WHERE u.username = 'analyst'`, 0)
	})

	t.Run("login issues an opaque cookie stored only as a hash", func(t *testing.T) {
		response := f.send(t, http.MethodPost, "/auth/login", "", map[string]string{"username": "Analyst", "password": memberPassword})
		if response.status != http.StatusOK || response.cookie == nil {
			t.Fatalf("login status = %d", response.status)
		}
		cookie := response.cookie
		if len(cookie.Value) < 43 || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.MaxAge <= 0 {
			t.Fatalf("session cookie attributes = %+v", cookie)
		}
		if bytes.Contains(response.body, []byte(cookie.Value)) {
			t.Fatal("login response body contains the session identifier")
		}
		assertCount(t, f.platform, `SELECT count(*) FROM access_sessions WHERE token_hash = $1 AND current_token_during_grace IS NULL`, 1, sessionHash(cookie.Value))
	})
}

func TestAccessControlSessionLifecycleIntegration(t *testing.T) {
	f := newFixture(t)
	admin := f.login(t, "admin", adminPassword)
	createAccessUser(t, admin, "analyst", false)

	t.Run("logout revokes the backend session", func(t *testing.T) {
		analyst := f.login(t, "analyst", memberPassword)
		token := analyst.sessionToken(t)
		analyst.do(t, http.MethodPost, "/auth/logout", nil, http.StatusNoContent)
		f.expectSession(t, token, http.StatusUnauthorized)
		assertCount(t, f.platform, `SELECT count(*) FROM access_sessions WHERE token_hash = $1`, 0, sessionHash(token))
	})

	t.Run("idle timeout ends the session", func(t *testing.T) {
		token := f.login(t, "analyst", memberPassword).sessionToken(t)
		f.updateSession(t, token, `last_seen_at = now() - interval '31 minutes'`)
		f.expectSession(t, token, http.StatusUnauthorized)
		assertCount(t, f.platform, `SELECT count(*) FROM access_sessions WHERE token_hash = $1`, 0, sessionHash(token))
	})

	t.Run("absolute expiration ends an active session", func(t *testing.T) {
		token := f.login(t, "analyst", memberPassword).sessionToken(t)
		f.updateSession(t, token, `absolute_expires_at = now() - interval '1 second', last_seen_at = now()`)
		f.expectSession(t, token, http.StatusUnauthorized)
	})

	t.Run("login replaces an existing identifier", func(t *testing.T) {
		analyst := f.login(t, "analyst", memberPassword)
		previous := analyst.sessionToken(t)
		analyst.do(t, http.MethodPost, "/auth/login", map[string]string{"username": "analyst", "password": memberPassword}, http.StatusOK)
		if analyst.sessionToken(t) == previous {
			t.Fatal("login kept the previous session identifier")
		}
		f.expectSession(t, previous, http.StatusUnauthorized)
	})

	t.Run("periodic rotation keeps the absolute expiry and a short grace period", func(t *testing.T) {
		token := f.login(t, "analyst", memberPassword).sessionToken(t)
		expiryBefore := f.sessionExpiry(t, token)
		f.updateSession(t, token, `rotated_at = now() - interval '16 minutes'`)

		first := f.send(t, http.MethodGet, "/auth/me", token, nil)
		if first.status != http.StatusOK || first.cookie == nil || first.cookie.Value == token {
			t.Fatal("due rotation did not deliver a new identifier")
		}
		rotated := first.cookie.Value
		concurrent := f.send(t, http.MethodGet, "/auth/me", token, nil)
		if concurrent.status != http.StatusOK || concurrent.cookie == nil || concurrent.cookie.Value != rotated {
			t.Fatal("previous identifier did not receive the replacement during the grace period")
		}
		if expiryAfter := f.sessionExpiry(t, rotated); !expiryAfter.Equal(expiryBefore) {
			t.Fatalf("rotation moved absolute expiry from %s to %s", expiryBefore, expiryAfter)
		}

		f.updateSession(t, token, `previous_expires_at = now() - interval '1 second'`)
		f.expectSession(t, token, http.StatusUnauthorized)
		f.expectSession(t, rotated, http.StatusOK)
	})
}

func TestAccessControlAuthorizationIntegration(t *testing.T) {
	f := newFixture(t)
	admin := f.login(t, "admin", adminPassword)
	databaseID := registerAccessDatabase(t, f, admin)
	accessPath := "/databases/" + databaseID + "/access"
	analystID := createAccessUser(t, admin, "analyst", false)
	readers := createAccessGroup(t, admin, "readers")
	editors := createAccessGroup(t, admin, "editors")
	grantAccess(t, admin, readers, databaseID, access.ViewOnly)
	grantAccess(t, admin, editors, databaseID, access.ViewEdit)
	analyst := f.login(t, "analyst", memberPassword)

	t.Run("effective access uses the strongest group grant", func(t *testing.T) {
		analyst.do(t, http.MethodGet, accessPath, nil, http.StatusForbidden)
		admin.do(t, http.MethodPut, "/admin/groups/"+readers+"/members/"+analystID, nil, http.StatusNoContent)
		assertPermission(t, analyst, accessPath, access.ViewOnly)
		admin.do(t, http.MethodPut, "/admin/groups/"+editors+"/members/"+analystID, nil, http.StatusNoContent)
		assertPermission(t, analyst, accessPath, access.ViewEdit)
	})

	t.Run("privilege increase rotates sessions without logging users out", func(t *testing.T) {
		auditorID := createAccessUser(t, admin, "auditor", false)
		auditor := f.login(t, "auditor", memberPassword)
		token := auditor.sessionToken(t)
		admin.do(t, http.MethodPut, "/admin/groups/"+readers+"/members/"+auditorID, nil, http.StatusNoContent)
		admin.do(t, http.MethodPut, "/admin/groups/"+readers+"/members/"+currentUserID(t, admin), nil, http.StatusNoContent)
		execFixture(t, f.platform, `UPDATE access_sessions SET previous_expires_at = now() - interval '1 second' WHERE previous_expires_at IS NOT NULL`)

		next := f.send(t, http.MethodGet, accessPath, token, nil)
		if next.status != http.StatusOK || next.cookie == nil || next.cookie.Value == token {
			t.Fatalf("session after privilege increase: status %d, rotated %t", next.status, next.cookie != nil)
		}
		admin.do(t, http.MethodGet, "/admin/users", nil, http.StatusOK)
		assertPermission(t, admin, accessPath, access.ViewOnly)
	})

	t.Run("platform admin status does not grant database access", func(t *testing.T) {
		createAccessUser(t, admin, "operator", true)
		operator := f.login(t, "operator", memberPassword)
		operator.do(t, http.MethodGet, "/admin/users", nil, http.StatusOK)
		operator.do(t, http.MethodGet, accessPath, nil, http.StatusForbidden)
	})

	t.Run("state changes require csrf token and same origin", func(t *testing.T) {
		group := map[string]string{"name": "blocked"}
		admin.do(t, http.MethodPost, "/admin/groups", group, http.StatusForbidden, func(r *http.Request) { r.Header.Del("X-CSRF-Token") })
		admin.do(t, http.MethodPost, "/admin/groups", group, http.StatusForbidden, func(r *http.Request) { r.Header.Set("X-CSRF-Token", "forged") })
		admin.do(t, http.MethodPost, "/admin/groups", group, http.StatusForbidden, func(r *http.Request) { r.Header.Set("Origin", "http://attacker.test") })
		admin.do(t, http.MethodPost, "/admin/groups", group, http.StatusForbidden, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
		assertCount(t, f.platform, `SELECT count(*) FROM access_groups WHERE name = 'blocked'`, 0)
		analyst.do(t, http.MethodGet, "/admin/users", nil, http.StatusForbidden)
		f.anonymous().do(t, http.MethodGet, "/admin/users", nil, http.StatusUnauthorized)
	})

	t.Run("access reductions revoke sessions immediately", func(t *testing.T) {
		grantAccess(t, admin, editors, databaseID, access.ViewOnly)
		analyst.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		analyst = f.login(t, "analyst", memberPassword)
		assertPermission(t, analyst, accessPath, access.ViewOnly)

		admin.do(t, http.MethodDelete, "/admin/groups/"+readers+"/members/"+analystID, nil, http.StatusNoContent)
		analyst.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		analyst = f.login(t, "analyst", memberPassword)

		admin.do(t, http.MethodDelete, "/admin/groups/"+editors+"/databases/"+databaseID, nil, http.StatusNoContent)
		analyst.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		analyst = f.login(t, "analyst", memberPassword)
		analyst.do(t, http.MethodGet, accessPath, nil, http.StatusForbidden)

		admin.do(t, http.MethodPatch, "/admin/users/"+analystID, map[string]bool{"active": false, "platform_admin": false}, http.StatusNoContent)
		analyst.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		f.anonymous().do(t, http.MethodPost, "/auth/login", map[string]string{"username": "analyst", "password": memberPassword}, http.StatusUnauthorized)
	})

	t.Run("deleting a group revokes member sessions", func(t *testing.T) {
		auditor := f.login(t, "auditor", memberPassword)
		admin.do(t, http.MethodDelete, "/admin/groups/"+readers, nil, http.StatusNoContent)
		auditor.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		admin.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		admin = f.login(t, "admin", adminPassword)
		admin.do(t, http.MethodGet, "/admin/groups/"+readers+"/members", nil, http.StatusNotFound)
	})

	t.Run("admin safeguards and validation", func(t *testing.T) {
		adminID := currentUserID(t, admin)
		admin.do(t, http.MethodPatch, "/admin/users/"+adminID, map[string]bool{"active": true, "platform_admin": false}, http.StatusConflict)

		operator := f.login(t, "operator", memberPassword)
		operatorID := currentUserID(t, operator)
		operator.do(t, http.MethodPatch, "/admin/users/"+adminID, map[string]bool{"active": true, "platform_admin": false}, http.StatusNoContent)
		admin.do(t, http.MethodGet, "/auth/me", nil, http.StatusUnauthorized)
		err := access.NewService(f.platform).UpdateUser(context.Background(), operatorID, false, true)
		if !errors.Is(err, access.ErrConflict) {
			t.Fatalf("disabling the last active admin returned %v", err)
		}

		var invalid struct {
			Error string `json:"error"`
		}
		decode(t, operator.do(t, http.MethodPost, "/admin/users", map[string]string{"username": "x", "password": memberPassword}, http.StatusBadRequest), &invalid)
		if invalid.Error != "username must contain 3 to 64 characters" {
			t.Fatalf("validation message = %q", invalid.Error)
		}
		operator.do(t, http.MethodPost, "/admin/users", map[string]string{"username": "auditor", "password": memberPassword}, http.StatusConflict)
		operator.do(t, http.MethodDelete, "/admin/groups/"+editors+"/members/"+operatorID, nil, http.StatusNotFound)
		operator.do(t, http.MethodDelete, "/admin/groups/"+editors+"/databases/"+databaseID, nil, http.StatusNotFound)
	})
}

func (c *apiClient) sessionToken(t *testing.T) string {
	t.Helper()
	base, err := url.Parse(c.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range c.client.Jar.Cookies(base) {
		if cookie.Name == sessionCookieName {
			return cookie.Value
		}
	}
	t.Fatal("client has no session cookie")
	return ""
}

func (f *fixture) send(t *testing.T, method, path, token string, input any) rawResponse {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatalf("integration HTTP request failed: %v", err)
	}
	defer response.Body.Close()
	result := rawResponse{status: response.StatusCode}
	if result.body, err = io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			result.cookie = cookie
		}
	}
	return result
}

func (f *fixture) expectSession(t *testing.T, token string, status int) {
	t.Helper()
	if response := f.send(t, http.MethodGet, "/auth/me", token, nil); response.status != status {
		t.Fatalf("session status = %d, want %d", response.status, status)
	}
}

func (f *fixture) updateSession(t *testing.T, token, assignment string) {
	t.Helper()
	execFixture(t, f.platform, `UPDATE access_sessions SET `+assignment+` WHERE token_hash = $1 OR previous_token_hash = $1`, sessionHash(token))
}

func (f *fixture) sessionExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	var expiry time.Time
	if err := f.platform.QueryRow(`SELECT absolute_expires_at FROM access_sessions WHERE token_hash = $1`, sessionHash(token)).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	return expiry
}

func sessionHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}

func createAccessUser(t *testing.T, admin *apiClient, username string, platformAdmin bool) string {
	t.Helper()
	var created struct {
		User access.User `json:"user"`
	}
	input := map[string]any{"username": username, "password": memberPassword, "platform_admin": platformAdmin}
	decode(t, admin.do(t, http.MethodPost, "/admin/users", input, http.StatusCreated), &created)
	return created.User.ID
}

func createAccessGroup(t *testing.T, admin *apiClient, name string) string {
	t.Helper()
	var created struct {
		Group access.Group `json:"group"`
	}
	decode(t, admin.do(t, http.MethodPost, "/admin/groups", map[string]string{"name": name}, http.StatusCreated), &created)
	return created.Group.ID
}

func grantAccess(t *testing.T, admin *apiClient, groupID, databaseID string, permission access.Permission) {
	t.Helper()
	admin.do(t, http.MethodPut, "/admin/groups/"+groupID+"/databases/"+databaseID, map[string]access.Permission{"permission": permission}, http.StatusNoContent)
}

func registerAccessDatabase(t *testing.T, f *fixture, admin *apiClient) string {
	t.Helper()
	var registered struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	decode(t, admin.do(t, http.MethodPost, "/admin/databases", registration("Access target", f.settings), http.StatusCreated), &registered)
	return registered.Database.ID
}

func currentUserID(t *testing.T, client *apiClient) string {
	t.Helper()
	var current struct {
		User access.User `json:"user"`
	}
	decode(t, client.do(t, http.MethodGet, "/auth/me", nil, http.StatusOK), &current)
	return current.User.ID
}

func assertPermission(t *testing.T, client *apiClient, path string, want access.Permission) {
	t.Helper()
	var effective struct {
		Permission access.Permission `json:"permission"`
	}
	decode(t, client.do(t, http.MethodGet, path, nil, http.StatusOK), &effective)
	if effective.Permission != want {
		t.Fatalf("effective permission = %q, want %q", effective.Permission, want)
	}
}
