package migration_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestNormalizeMigrationName(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"trim surrounding whitespace", " \tcreate customers\n", "create customers"},
		{"preserve unicode", "tambah pelanggan 日本語", "tambah pelanggan 日本語"},
		{"maximum bytes", strings.Repeat("a", migration.MaxMigrationNameBytes), strings.Repeat("a", migration.MaxMigrationNameBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := migration.NormalizeMigrationName(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeMigrationName() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, input string
	}{
		{"empty", ""},
		{"whitespace", " \t\r\n"},
		{"too long", strings.Repeat("a", migration.MaxMigrationNameBytes+1)},
		{"unicode exceeds byte limit", strings.Repeat("界", 54)},
		{"NUL", "private\x00name"},
		{"invalid UTF-8", string([]byte{0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := migration.NormalizeMigrationName(tc.input)
			if !errors.Is(err, migration.ErrInvalidMigration) || got != "" {
				t.Fatalf("NormalizeMigrationName() = %q, %v; want invalid migration", got, err)
			}
		})
	}
}

func validRevision() migration.Revision {
	return migration.Revision{
		MigrationID: "00000000-0000-4000-8000-000000000001",
		Number:      1,
		SQLUp:       "\n-- Apply change\nCREATE TABLE customers (id bigint);\n",
		SQLDown:     "\n-- Undo change\nDROP TABLE customers;\n",
	}
}

func TestNormalizeRevision(t *testing.T) {
	for _, irreversible := range []bool{false, true} {
		name := "reversible"
		revision := validRevision()
		if irreversible {
			name = "irreversible"
			revision.Irreversible = true
			revision.IrreversibleReason = " \tdeleted data cannot be recovered\n"
			revision.SQLDown = ""
		}
		t.Run(name, func(t *testing.T) {
			original := revision
			got, err := migration.NormalizeRevision(revision)
			if err != nil {
				t.Fatal(err)
			}
			want := revision
			want.IrreversibleReason = strings.TrimSpace(want.IrreversibleReason)
			if got != want || revision != original {
				t.Fatalf("NormalizeRevision() changed SQL or input: got %+v, want %+v", got, want)
			}
		})
	}
	for _, field := range []string{"up", "down"} {
		t.Run("maximum SQL bytes "+field, func(t *testing.T) {
			revision := validRevision()
			statement := "SELECT 1;" + strings.Repeat(" ", migration.MaxSQLBytes-len("SELECT 1;"))
			if field == "up" {
				revision.SQLUp = statement
			} else {
				revision.SQLDown = statement
			}
			got, err := migration.NormalizeRevision(revision)
			if err != nil || got != revision {
				t.Fatal("maximum-size SQL was rejected or changed:", err)
			}
		})
	}
}

func TestNormalizeRevisionRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*migration.Revision)
	}{
		{"zero revision", func(r *migration.Revision) { r.Number = 0 }},
		{"negative revision", func(r *migration.Revision) { r.Number = -1 }},
		{"missing up", func(r *migration.Revision) { r.SQLUp = "" }},
		{"whitespace up", func(r *migration.Revision) { r.SQLUp = " \t\n" }},
		{"missing down", func(r *migration.Revision) { r.SQLDown = "" }},
		{"whitespace down", func(r *migration.Revision) { r.SQLDown = " \t\n" }},
		{"oversized up", func(r *migration.Revision) { r.SQLUp = strings.Repeat("a", migration.MaxSQLBytes+1) }},
		{"oversized down", func(r *migration.Revision) { r.SQLDown = strings.Repeat("a", migration.MaxSQLBytes+1) }},
		{"NUL up", func(r *migration.Revision) { r.SQLUp = "private SQL\x00" }},
		{"NUL down", func(r *migration.Revision) { r.SQLDown = "private SQL\x00" }},
		{"invalid UTF-8 up", func(r *migration.Revision) { r.SQLUp = string([]byte{0xff}) }},
		{"invalid UTF-8 down", func(r *migration.Revision) { r.SQLDown = string([]byte{0xff}) }},
		{"reason on reversible migration", func(r *migration.Revision) { r.IrreversibleReason = "private reason" }},
		{"irreversible without reason", func(r *migration.Revision) {
			r.Irreversible, r.SQLDown = true, ""
		}},
		{"irreversible with blank reason", func(r *migration.Revision) {
			r.Irreversible, r.SQLDown, r.IrreversibleReason = true, "", " \t\n"
		}},
		{"irreversible with down SQL", func(r *migration.Revision) {
			r.Irreversible, r.IrreversibleReason = true, "private reason"
		}},
		{"irreversible with whitespace down", func(r *migration.Revision) {
			r.Irreversible, r.SQLDown, r.IrreversibleReason = true, " \t\n", "private reason"
		}},
		{"NUL reason", func(r *migration.Revision) {
			r.Irreversible, r.SQLDown, r.IrreversibleReason = true, "", "private reason\x00"
		}},
		{"invalid UTF-8 reason", func(r *migration.Revision) {
			r.Irreversible, r.SQLDown, r.IrreversibleReason = true, "", string([]byte{0xff})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision := validRevision()
			tc.change(&revision)
			got, err := migration.NormalizeRevision(revision)
			if !errors.Is(err, migration.ErrInvalidMigration) || got != (migration.Revision{}) {
				t.Fatalf("NormalizeRevision() = %+v, %v; want invalid migration", got, err)
			}
			if strings.Contains(err.Error(), "private SQL") || strings.Contains(err.Error(), "private reason") {
				t.Fatal("validation error exposed input contents")
			}
		})
	}
}

func TestRevisionSQLFor(t *testing.T) {
	for _, tc := range []struct {
		name         string
		action       migration.Action
		irreversible bool
		wantSQL      string
		wantError    error
	}{
		{"apply", migration.Apply, false, validRevision().SQLUp, nil},
		{"forward", migration.Forward, false, validRevision().SQLUp, nil},
		{"rollback", migration.Rollback, false, validRevision().SQLDown, nil},
		{"irreversible apply", migration.Apply, true, validRevision().SQLUp, nil},
		{"irreversible forward", migration.Forward, true, validRevision().SQLUp, nil},
		{"irreversible rollback", migration.Rollback, true, "", migration.ErrIrreversibleMigration},
		{"empty action", "", false, "", migration.ErrInvalidMigration},
		{"unknown action", "retry", false, "", migration.ErrInvalidMigration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision := validRevision()
			if tc.irreversible {
				revision.Irreversible, revision.SQLDown, revision.IrreversibleReason = true, "", "deleted data cannot be recovered"
			}
			got, err := revision.SQLFor(tc.action)
			if !errors.Is(err, tc.wantError) || got != tc.wantSQL {
				t.Fatalf("SQLFor() = %q, %v; want %q, %v", got, err, tc.wantSQL, tc.wantError)
			}
		})
	}
	t.Run("invalid revision", func(t *testing.T) {
		if _, err := (migration.Revision{}).SQLFor(migration.Apply); !errors.Is(err, migration.ErrInvalidMigration) {
			t.Fatalf("SQLFor() error = %v; want invalid migration", err)
		}
	})
	if !errors.Is(migration.ErrIrreversibleMigration, migration.ErrInvalidMigration) {
		t.Fatal("irreversible errors must remain compatible with invalid migration handling")
	}
}

func TestRecordPreservesLegacyJSON(t *testing.T) {
	record := migration.Record{
		Version:    1,
		Name:       "create customers",
		SQL:        "SELECT 1",
		Status:     migration.Success,
		ExecutedBy: "00000000-0000-4000-8000-000000000001",
		StartedAt:  time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"name":"create customers","sql":"SELECT 1","status":"success","executed_by":"00000000-0000-4000-8000-000000000001","started_at":"2026-11-08T00:00:00Z"}`
	if string(data) != want {
		t.Fatalf("legacy response = %s; want %s", data, want)
	}
}
