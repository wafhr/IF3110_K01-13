package migration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestExecuteValidatesBeforeOpeningTarget(t *testing.T) {
	for _, tc := range []struct {
		name, migrationName, statement string
		wantError                      error
		wantOpens                      int
	}{
		{"empty name", "", "SELECT 1", migration.ErrInvalidMigration, 0},
		{"oversized name", strings.Repeat("a", migration.MaxMigrationNameBytes+1), "SELECT 1", migration.ErrInvalidMigration, 0},
		{"NUL name", "private\x00name", "SELECT 1", migration.ErrInvalidMigration, 0},
		{"invalid UTF-8 name", string([]byte{0xff}), "SELECT 1", migration.ErrInvalidMigration, 0},
		{"empty SQL", "preview", " \t\n", migration.ErrInvalidMigration, 0},
		{"oversized SQL", "preview", strings.Repeat("a", migration.MaxSQLBytes+1), migration.ErrInvalidMigration, 0},
		{"NUL SQL", "preview", "SELECT 1\x00", migration.ErrInvalidMigration, 0},
		{"invalid UTF-8 SQL", "preview", string([]byte{0xff}), migration.ErrInvalidMigration, 0},
		{"legacy SQL without down", " preview ", " SELECT 1 ", context.Canceled, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens := 0
			service := migration.NewService(func(_ context.Context, databaseID string) (*sql.DB, database.DBMS, error) {
				opens++
				if databaseID != "target-id" {
					t.Errorf("target database ID = %q", databaseID)
				}
				return nil, database.PostgreSQL, context.Canceled
			})
			record, err := service.Execute(context.Background(), "target-id", "user-id", tc.migrationName, tc.statement)
			if !errors.Is(err, tc.wantError) || opens != tc.wantOpens || record != (migration.Record{}) {
				t.Fatalf("Execute() = %+v, %v; target opens = %d; want %v and %d opens", record, err, opens, tc.wantError, tc.wantOpens)
			}
		})
	}
}
