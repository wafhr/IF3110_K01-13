package access

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

const (
	registeredDatabase = "FROM managed_databases"
	existingGrant      = "SELECT permission FROM access_group_database_permissions"
	grantDelete        = "DELETE FROM access_group_database_permissions"
	grantUpsert        = "INSERT INTO access_group_database_permissions"
)

func TestPermissionSatisfies(t *testing.T) {
	tests := []struct {
		effective, minimum Permission
		want               bool
	}{
		{effective: ViewOnly, minimum: ViewOnly, want: true},
		{effective: ViewOnly, minimum: ViewEdit},
		{effective: ViewEdit, minimum: ViewOnly, want: true},
		{effective: ViewEdit, minimum: ViewEdit, want: true},
		{effective: "", minimum: ViewOnly},
		{effective: ViewEdit, minimum: ""},
		{effective: ViewEdit, minimum: "owner"},
		{effective: "owner", minimum: ViewOnly},
	}
	for _, test := range tests {
		if got := test.effective.satisfies(test.minimum); got != test.want {
			t.Errorf("%q satisfies %q = %t, want %t", test.effective, test.minimum, got, test.want)
		}
	}
}

func TestAuthorizeDatabase(t *testing.T) {
	tests := []struct {
		name     string
		userID   string
		minimum  Permission
		database *fakeDatabase
		wantErr  error
	}{
		{name: "unknown minimum", userID: testUserID, minimum: "owner", database: newFakeDatabase(), wantErr: ErrInvalidInput},
		{name: "invalid user id", userID: "user", minimum: ViewOnly, database: newFakeDatabase(), wantErr: ErrNotFound},
		{name: "no grant", userID: testUserID, minimum: ViewOnly, database: newFakeDatabase().withPermission(""), wantErr: ErrForbidden},
		{name: "view only reads", userID: testUserID, minimum: ViewOnly, database: newFakeDatabase().withPermission(ViewOnly)},
		{name: "view only cannot edit", userID: testUserID, minimum: ViewEdit, database: newFakeDatabase().withPermission(ViewOnly), wantErr: ErrForbidden},
		{name: "view edit edits", userID: testUserID, minimum: ViewEdit, database: newFakeDatabase().withPermission(ViewEdit)},
		{name: "lookup failure", userID: testUserID, minimum: ViewOnly, database: newFakeDatabase().failQuery(permissionLookup, errDatabaseDown), wantErr: errDatabaseDown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newTestService(t, test.database, testNow).AuthorizeDatabase(context.Background(), test.userID, testDatabaseID, test.minimum)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSetDatabasePermission(t *testing.T) {
	registered := func(previous Permission) *fakeDatabase {
		database := newFakeDatabase().onQuery(registeredDatabase, []string{"id"}, []driver.Value{testDatabaseID})
		if previous == "" {
			return database.onQuery(existingGrant, []string{"permission"})
		}
		return database.onQuery(existingGrant, []string{"permission"}, []driver.Value{string(previous)})
	}
	tests := []struct {
		name       string
		databaseID string
		permission Permission
		database   *fakeDatabase
		wantErr    error
		wantEffect sessionEffect
	}{
		{name: "invalid database id", databaseID: "db", permission: ViewOnly, database: newFakeDatabase(), wantErr: ErrNotFound},
		{name: "unknown permission", databaseID: testDatabaseID, permission: "owner", database: newFakeDatabase(), wantErr: ErrInvalidInput},
		{name: "unregistered database", databaseID: testDatabaseID, permission: ViewOnly, database: newFakeDatabase().onQuery(registeredDatabase, []string{"id"}), wantErr: ErrNotFound},
		{name: "unknown group", databaseID: testDatabaseID, permission: ViewOnly, database: registered("").failExec(grantUpsert, sqlStateError("23503")), wantErr: ErrNotFound},
		{name: "new grant rotates sessions", databaseID: testDatabaseID, permission: ViewOnly, database: registered(""), wantEffect: sessionsRotated},
		{name: "upgrade rotates sessions", databaseID: testDatabaseID, permission: ViewEdit, database: registered(ViewOnly), wantEffect: sessionsRotated},
		{name: "downgrade revokes sessions", databaseID: testDatabaseID, permission: ViewOnly, database: registered(ViewEdit), wantEffect: sessionsRevoked},
		{name: "same grant keeps sessions", databaseID: testDatabaseID, permission: ViewEdit, database: registered(ViewEdit)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newTestService(t, test.database, testNow).SetDatabasePermission(context.Background(), testGroupID, test.databaseID, test.permission)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			assertSessionEffect(t, test.database, groupRotation, groupRevocation, test.wantEffect)
		})
	}
}

func TestDeleteDatabasePermission(t *testing.T) {
	missing := newFakeDatabase().onExec(grantDelete, 0)
	if err := newTestService(t, missing, testNow).DeleteDatabasePermission(context.Background(), testGroupID, testDatabaseID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing grant returned %v", err)
	}
	assertSessionEffect(t, missing, groupRotation, groupRevocation, sessionsUnchanged)

	existing := newFakeDatabase()
	if err := newTestService(t, existing, testNow).DeleteDatabasePermission(context.Background(), testGroupID, testDatabaseID); err != nil {
		t.Fatal(err)
	}
	assertSessionEffect(t, existing, groupRotation, groupRevocation, sessionsRevoked)
}
