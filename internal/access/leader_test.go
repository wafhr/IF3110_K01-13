package access

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestSetGroupLeaderRequiresActiveMember(t *testing.T) {
	database := newFakeDatabase().
		onQuery("SELECT id::text FROM access_groups WHERE id = $1 FOR UPDATE", []string{"id"}, []driver.Value{testGroupID}).
		onQuery("SELECT EXISTS (", []string{"exists"}, []driver.Value{false})
	err := newTestService(t, database, testNow).SetGroupLeader(context.Background(), testGroupID, testUserID)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-member leader assignment error = %v, want ErrInvalidInput", err)
	}
	if database.ran("UPDATE access_groups SET leader_user_id") {
		t.Fatal("rejected leader assignment was written")
	}
}

func TestSetGroupLeaderAcceptsActiveMemberAndClear(t *testing.T) {
	database := newFakeDatabase().
		onQuery("SELECT id::text FROM access_groups WHERE id = $1 FOR UPDATE", []string{"id"}, []driver.Value{testGroupID}).
		onQuery("SELECT EXISTS (", []string{"exists"}, []driver.Value{true})
	service := newTestService(t, database, testNow)
	if err := service.SetGroupLeader(context.Background(), testGroupID, testUserID); err != nil {
		t.Fatalf("assign active member: %v", err)
	}
	if !database.ran("UPDATE access_groups SET leader_user_id") {
		t.Fatal("leader assignment was not written")
	}
	if err := service.SetGroupLeader(context.Background(), testGroupID, ""); err != nil {
		t.Fatalf("clear leader: %v", err)
	}
}

func TestSetGroupLeaderRejectsInvalidGroup(t *testing.T) {
	err := newTestService(t, newFakeDatabase(), testNow).SetGroupLeader(context.Background(), "bad", testUserID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid group ID error = %v, want ErrNotFound", err)
	}
}
