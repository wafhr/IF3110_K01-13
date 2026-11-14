package access

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

const (
	loginLookup      = "FROM access_users WHERE username"
	sessionInsert    = "INSERT INTO access_sessions"
	sessionRotation  = "SET previous_token_hash = token_hash"
	sessionTouch     = "UPDATE access_sessions SET last_seen_at"
	sessionExpiry    = "DELETE FROM access_sessions WHERE token_hash = $1"
	testUserPassword = "reader-password-123"
)

var (
	testNow      = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	loginColumns = []string{"id", "username", "password_hash", "is_platform_admin", "is_active", "created_at"}
)

func loginRow(t *testing.T, active bool) []driver.Value {
	t.Helper()
	hash, err := hashPassword(testUserPassword)
	if err != nil {
		t.Fatal(err)
	}
	return []driver.Value{testUserID, testUsername, hash, false, active, testNow}
}

func TestLoginRejectsInvalidCredentials(t *testing.T) {
	tests := []struct {
		name     string
		database *fakeDatabase
		username string
		password string
	}{
		{name: "malformed username", database: newFakeDatabase(), username: "a b", password: testUserPassword},
		{name: "empty password", database: newFakeDatabase(), username: testUsername},
		{name: "unknown user", database: newFakeDatabase().onQuery(loginLookup, loginColumns), username: "ghost", password: testUserPassword},
		{name: "wrong password", database: newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, true)), username: testUsername, password: "another-password-1"},
		{name: "inactive user", database: newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, false)), username: testUsername, password: testUserPassword},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTestService(t, test.database, testNow).Login(context.Background(), test.username, test.password)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want unauthenticated", err)
			}
			if test.database.ran(sessionInsert) {
				t.Fatal("rejected login created a session")
			}
		})
	}
}

func TestLoginStoresOnlyTokenHash(t *testing.T) {
	database := newFakeDatabase().onQuery(loginLookup, loginColumns, loginRow(t, true))
	session, err := newTestService(t, database, testNow).Login(context.Background(), "  READER ", testUserPassword)
	if err != nil {
		t.Fatal(err)
	}
	if session.User.ID != testUserID || session.RotatedToken == "" || session.CSRFToken == "" || session.CSRFToken == session.RotatedToken {
		t.Fatalf("unexpected session %+v", session)
	}
	if !session.AbsoluteExpiry.Equal(testNow.Add(absoluteLifetime)) {
		t.Fatalf("absolute expiry = %s", session.AbsoluteExpiry)
	}
	inserts := database.executed(sessionInsert)
	if len(inserts) != 1 {
		t.Fatalf("session inserts = %d, want 1", len(inserts))
	}
	if hash, ok := inserts[0].args[0].([]byte); !ok || !bytes.Equal(hash, tokenHash(session.RotatedToken)) {
		t.Fatal("session identifier was not stored as its hash")
	}
	for _, arg := range inserts[0].args {
		if arg == session.RotatedToken {
			t.Fatal("plaintext session identifier was stored")
		}
	}
	if !database.ran("DELETE FROM access_sessions WHERE absolute_expires_at") {
		t.Fatal("login did not remove stale sessions")
	}
}

func TestLoginReportsDatabaseFailure(t *testing.T) {
	database := newFakeDatabase().failQuery(loginLookup, errDatabaseDown)
	_, err := newTestService(t, database, testNow).Login(context.Background(), testUsername, testUserPassword)
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error = %v, want internal failure", err)
	}
}

func TestAuthenticateRejectsUnusableSessions(t *testing.T) {
	expired := func(change func(*sessionRow)) *fakeDatabase {
		row := activeSession(testNow)
		change(&row)
		return newFakeDatabase().withSession(row)
	}
	tests := []struct {
		name        string
		token       string
		database    *fakeDatabase
		wantRemoval bool
	}{
		{name: "missing token", database: newFakeDatabase()},
		{name: "unknown token", token: testSessionToken, database: newFakeDatabase().withoutSession()},
		{name: "idle timeout", token: testSessionToken, wantRemoval: true, database: expired(func(r *sessionRow) { r.lastSeen = testNow.Add(-idleTimeout) })},
		{name: "absolute expiration", token: testSessionToken, wantRemoval: true, database: expired(func(r *sessionRow) { r.absoluteExpiry = testNow })},
		{name: "disabled user", token: testSessionToken, wantRemoval: true, database: expired(func(r *sessionRow) { r.inactive = true })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTestService(t, test.database, testNow).Authenticate(context.Background(), test.token)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want unauthenticated", err)
			}
			if test.database.ran(sessionExpiry) != test.wantRemoval {
				t.Fatalf("session removed = %t, want %t", !test.wantRemoval, test.wantRemoval)
			}
			if test.database.ran(sessionTouch) || test.database.ran(sessionRotation) {
				t.Fatal("rejected session was refreshed")
			}
		})
	}
}

func TestAuthenticateRotation(t *testing.T) {
	tests := []struct {
		name        string
		change      func(*sessionRow)
		wantRotate  bool
		wantPending string
	}{
		{name: "recent identifier is kept", change: func(*sessionRow) {}},
		{name: "rotation interval elapsed", wantRotate: true, change: func(r *sessionRow) { r.rotatedAt = testNow.Add(-rotationInterval) }},
		{name: "privilege change requires rotation", wantRotate: true, change: func(r *sessionRow) { r.rotationRequired = true }},
		{
			name:        "previous identifier receives replacement during grace",
			wantPending: "replacement-token",
			change: func(r *sessionRow) {
				r.current, r.graceToken, r.graceExpiry = false, "replacement-token", testNow.Add(rotationGrace)
			},
		},
		{
			name:        "previous identifier never starts another rotation",
			wantPending: "replacement-token",
			change: func(r *sessionRow) {
				r.current, r.rotationRequired, r.graceToken, r.graceExpiry = false, true, "replacement-token", testNow.Add(time.Second)
			},
		},
		{
			name: "expired replacement is not returned",
			change: func(r *sessionRow) {
				r.current, r.graceToken, r.graceExpiry = false, "replacement-token", testNow
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := activeSession(testNow)
			test.change(&row)
			database := newFakeDatabase().withSession(row)
			session, err := newTestService(t, database, testNow).Authenticate(context.Background(), testSessionToken)
			if err != nil {
				t.Fatal(err)
			}
			if session.User.ID != testUserID || session.CSRFToken != testCSRFToken {
				t.Fatalf("unexpected session %+v", session)
			}
			rotations := database.executed(sessionRotation)
			if test.wantRotate {
				assertRotation(t, rotations, session.RotatedToken)
				return
			}
			if len(rotations) != 0 || !database.ran(sessionTouch) {
				t.Fatal("session should only refresh its activity")
			}
			if session.RotatedToken != test.wantPending {
				t.Fatalf("rotated token = %q, want %q", session.RotatedToken, test.wantPending)
			}
		})
	}
}

func assertRotation(t *testing.T, rotations []statement, token string) {
	t.Helper()
	if len(rotations) != 1 || token == "" || token == testSessionToken {
		t.Fatalf("rotation statements = %d, token rotated = %t", len(rotations), token != testSessionToken)
	}
	args := rotations[0].args
	if graceEnd, ok := args[0].(time.Time); !ok || !graceEnd.Equal(testNow.Add(rotationGrace)) {
		t.Fatalf("grace period ends at %v", args[0])
	}
	if hash, ok := args[1].([]byte); !ok || !bytes.Equal(hash, tokenHash(token)) {
		t.Fatal("rotated identifier was not stored as its hash")
	}
}

func TestLogoutRevokesByHash(t *testing.T) {
	database := newFakeDatabase()
	service := newTestService(t, database, testNow)
	if err := service.Logout(context.Background(), ""); err != nil || len(database.executed("")) != 0 {
		t.Fatal("logout without a token touched the database")
	}
	if err := service.Logout(context.Background(), testSessionToken); err != nil {
		t.Fatal(err)
	}
	revocations := database.executed("DELETE FROM access_sessions WHERE token_hash = $1 OR previous_token_hash = $1")
	if len(revocations) != 1 || !bytes.Equal(revocations[0].args[0].([]byte), tokenHash(testSessionToken)) {
		t.Fatal("logout did not revoke the current and previous identifier by hash")
	}
}
