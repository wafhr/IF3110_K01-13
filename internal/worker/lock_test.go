package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestLockDatabase(t *testing.T) {
	target := newStubTarget()
	conn, err := target.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := lockDatabase(context.Background(), conn, "database"); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatalf("malformed database ID returned %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	unlock, err := lockDatabase(ctx, conn, testDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	unlock()
	if !target.ran("pg_advisory_lock") || !target.ran("pg_advisory_unlock") {
		t.Fatalf("statements = %q", target.order())
	}

	busy := newStubTarget()
	busy.execErrors["pg_advisory_lock"] = context.DeadlineExceeded
	busyConn, err := busy.open(t).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer busyConn.Close()
	if _, err := lockDatabase(context.Background(), busyConn, testDatabaseID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock failure returned %v", err)
	}
}
