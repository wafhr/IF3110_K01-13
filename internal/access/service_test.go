package access

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

const (
	userInsert       = "INSERT INTO access_users"
	userLookup       = "SELECT is_active, is_platform_admin FROM access_users"
	adminCount       = "WHERE is_active AND is_platform_admin"
	userUpdate       = "UPDATE access_users SET"
	groupInsert      = "INSERT INTO access_groups"
	groupExists      = "SELECT EXISTS (SELECT 1 FROM access_groups"
	memberInsert     = "INSERT INTO access_group_members"
	memberDelete     = "DELETE FROM access_group_members"
	groupDelete      = "DELETE FROM access_groups"
	userRevocation   = "DELETE FROM access_sessions WHERE " + string(userSessions)
	groupRevocation  = "DELETE FROM access_sessions WHERE " + string(groupSessions)
	userRotation     = "UPDATE access_sessions SET rotation_required = TRUE WHERE " + string(userSessions)
	groupRotation    = "UPDATE access_sessions SET rotation_required = TRUE WHERE " + string(groupSessions)
	unknownID        = "00000000-0000-4000-8000-0000000000ff"
	validNewPassword = "a-long-enough-password"
)

var (
	userColumns    = []string{"id", "username", "is_platform_admin", "is_active", "created_at"}
	accountColumns = []string{"is_active", "is_platform_admin"}
)

type sessionEffect int

const (
	sessionsUnchanged sessionEffect = iota
	sessionsRotated
	sessionsRevoked
)

func assertSessionEffect(t *testing.T, database *fakeDatabase, rotation, revocation string, want sessionEffect) {
	t.Helper()
	executed := func(query string) bool {
		for _, s := range database.executed(query) {
			if s.query == query {
				return true
			}
		}
		return false
	}
	rotated, revoked := executed(rotation), executed(revocation)
	if rotated != (want == sessionsRotated) || revoked != (want == sessionsRevoked) {
		t.Fatalf("sessions rotated = %t, revoked = %t; want effect %d", rotated, revoked, want)
	}
}

func TestCreateUser(t *testing.T) {
	created := []driver.Value{testUserID, "new.user", false, true, testNow}
	tests := []struct {
		name     string
		username string
		password string
		database *fakeDatabase
		wantErr  error
	}{
		{name: "invalid username", username: "x", password: validNewPassword, database: newFakeDatabase(), wantErr: ErrInvalidInput},
		{name: "short password", username: "new.user", password: "short", database: newFakeDatabase(), wantErr: ErrInvalidInput},
		{name: "duplicate username", username: "new.user", password: validNewPassword, database: newFakeDatabase().failQuery(userInsert, sqlStateError("23505")), wantErr: ErrConflict},
		{name: "database failure", username: "new.user", password: validNewPassword, database: newFakeDatabase().failQuery(userInsert, errDatabaseDown), wantErr: errDatabaseDown},
		{name: "created", username: " New.User ", password: validNewPassword, database: newFakeDatabase().onQuery(userInsert, userColumns, created)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, err := newTestService(t, test.database, testNow).CreateUser(context.Background(), test.username, test.password, false)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			inserts := test.database.executed(userInsert)
			if user.Username != "new.user" || len(inserts) != 1 || inserts[0].args[0] != "new.user" {
				t.Fatalf("username was not normalized: %+v", user)
			}
			if stored, _ := inserts[0].args[1].(string); stored == test.password || !verifyPassword(stored, test.password) {
				t.Fatal("password was not stored as a verifiable hash")
			}
		})
	}
}

func TestCreateGroup(t *testing.T) {
	created := []driver.Value{testGroupID, "Data Team", testNow}
	tests := []struct {
		name     string
		input    string
		database *fakeDatabase
		wantErr  error
	}{
		{name: "blank name", input: "  ", database: newFakeDatabase(), wantErr: ErrInvalidInput},
		{name: "duplicate name", input: "Data Team", database: newFakeDatabase().failQuery(groupInsert, sqlStateError("23505")), wantErr: ErrConflict},
		{name: "created", input: " Data Team ", database: newFakeDatabase().onQuery(groupInsert, []string{"id", "name", "created_at"}, created)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			group, err := newTestService(t, test.database, testNow).CreateGroup(context.Background(), test.input)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && test.database.executed(groupInsert)[0].args[0] != "Data Team" {
				t.Fatalf("group name was not trimmed: %+v", group)
			}
		})
	}
}

func TestUpdateUser(t *testing.T) {
	account := func(active, admin bool) []driver.Value { return []driver.Value{active, admin} }
	admins := func(count int64) []driver.Value { return []driver.Value{count} }
	tests := []struct {
		name          string
		id            string
		active, admin bool
		database      *fakeDatabase
		wantErr       error
		wantEffect    sessionEffect
	}{
		{name: "invalid id", id: "not-a-uuid", database: newFakeDatabase(), wantErr: ErrNotFound},
		{name: "unknown user", id: unknownID, database: newFakeDatabase().onQuery(userLookup, accountColumns), wantErr: ErrNotFound},
		{
			name: "last active admin cannot be demoted", id: testUserID, active: true,
			database: newFakeDatabase().onQuery(userLookup, accountColumns, account(true, true)).onQuery(adminCount, []string{"count"}, admins(1)),
			wantErr:  ErrConflict,
		},
		{
			name: "demotion revokes sessions", id: testUserID, active: true,
			database:   newFakeDatabase().onQuery(userLookup, accountColumns, account(true, true)).onQuery(adminCount, []string{"count"}, admins(2)),
			wantEffect: sessionsRevoked,
		},
		{
			name: "deactivation revokes sessions", id: testUserID,
			database:   newFakeDatabase().onQuery(userLookup, accountColumns, account(true, false)),
			wantEffect: sessionsRevoked,
		},
		{
			name: "promotion rotates sessions", id: testUserID, active: true, admin: true,
			database:   newFakeDatabase().onQuery(userLookup, accountColumns, account(true, false)),
			wantEffect: sessionsRotated,
		},
		{
			name: "unchanged flags keep sessions", id: testUserID, active: true,
			database: newFakeDatabase().onQuery(userLookup, accountColumns, account(true, false)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newTestService(t, test.database, testNow).UpdateUser(context.Background(), test.id, test.active, test.admin)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil && test.database.ran(userUpdate) {
				t.Fatal("rejected update changed the user")
			}
			assertSessionEffect(t, test.database, userRotation, userRevocation, test.wantEffect)
		})
	}
}

func TestSetMembership(t *testing.T) {
	tests := []struct {
		name       string
		groupID    string
		member     bool
		database   *fakeDatabase
		wantErr    error
		wantEffect sessionEffect
	}{
		{name: "invalid group id", groupID: "group", member: true, database: newFakeDatabase(), wantErr: ErrNotFound},
		{name: "join rotates sessions", groupID: testGroupID, member: true, database: newFakeDatabase(), wantEffect: sessionsRotated},
		{name: "existing member is unchanged", groupID: testGroupID, member: true, database: newFakeDatabase().onExec(memberInsert, 0)},
		{name: "unknown user or group", groupID: testGroupID, member: true, database: newFakeDatabase().failExec(memberInsert, sqlStateError("23503")), wantErr: ErrNotFound},
		{name: "leave revokes sessions", groupID: testGroupID, database: newFakeDatabase(), wantEffect: sessionsRevoked},
		{name: "non member cannot leave", groupID: testGroupID, database: newFakeDatabase().onExec(memberDelete, 0), wantErr: ErrNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newTestService(t, test.database, testNow).SetMembership(context.Background(), test.groupID, testUserID, test.member)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			assertSessionEffect(t, test.database, userRotation, userRevocation, test.wantEffect)
		})
	}
}

func TestDeleteGroup(t *testing.T) {
	missing := newFakeDatabase().onExec(groupDelete, 0)
	if err := newTestService(t, missing, testNow).DeleteGroup(context.Background(), testGroupID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing group returned %v", err)
	}
	existing := newFakeDatabase()
	if err := newTestService(t, existing, testNow).DeleteGroup(context.Background(), testGroupID); err != nil {
		t.Fatal(err)
	}
	assertSessionEffect(t, existing, groupRotation, groupRevocation, sessionsRevoked)
}

func TestListingUnknownGroupIsNotFound(t *testing.T) {
	database := newFakeDatabase().onQuery(groupExists, []string{"exists"}, []driver.Value{false})
	service := newTestService(t, database, testNow)
	if _, err := service.ListGroupMembers(context.Background(), unknownID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("members of unknown group: %v", err)
	}
	if _, err := service.ListDatabaseGrants(context.Background(), unknownID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grants of unknown group: %v", err)
	}
}
