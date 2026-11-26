package migration_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func timelineExecution(sequence, migrationNumber int, action migration.Action) migration.Execution {
	execution := committedExecution(sequence, action)
	execution.MigrationID = executionID(100 + migrationNumber)
	execution.SQL = validRevision().SQLUp
	if action == migration.Rollback {
		execution.SQL = validRevision().SQLDown
	}
	return execution
}

func timelineDefinition(execution migration.Execution) migration.Migration {
	return migration.Migration{ID: execution.MigrationID, DatabaseID: execution.DatabaseID,
		Name: execution.Name, CurrentRevision: execution.Revision}
}

func timelineRevision(execution migration.Execution) migration.Revision {
	revision := validRevision()
	revision.MigrationID, revision.Number = execution.MigrationID, execution.Revision
	return revision
}

func rolledBackTimeline() []migration.Execution {
	a := timelineExecution(1, 1, migration.Apply)
	b := timelineExecution(2, 2, migration.Apply)
	c := timelineExecution(3, 3, migration.Apply)
	rollbackC := timelineExecution(4, 3, migration.Rollback)
	rollbackC.RevertsExecutionID = c.ID
	rollbackB := timelineExecution(5, 2, migration.Rollback)
	rollbackB.RevertsExecutionID = b.ID
	return []migration.Execution{a, b, c, rollbackC, rollbackB}
}

func timelineIDs(executions []migration.Execution) []string {
	ids := make([]string, len(executions))
	for index, execution := range executions {
		ids[index] = execution.ID
	}
	return ids
}

func TestBuildMigrationStateReplaysRollbackAndForward(t *testing.T) {
	history := rolledBackTimeline()
	forwardB := timelineExecution(6, 2, migration.Forward)
	forwardC := timelineExecution(7, 3, migration.Forward)
	history = append(history, forwardB, forwardC)
	history[0], history[6] = history[6], history[0]
	original := append([]migration.Execution(nil), history...)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, history)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{executionID(1), forwardB.ID, forwardC.ID}
	if !reflect.DeepEqual(timelineIDs(state.Active), want) || len(state.PendingForward) != 0 || state.BaseExecutionID != forwardC.ID {
		t.Fatalf("posisi migration tidak sesuai: %+v", state)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("penyusunan posisi mengubah history masukan")
	}
}

func TestFailedAttemptsDoNotMoveMigrationState(t *testing.T) {
	history := rolledBackTimeline()[:3]
	failed := timelineExecution(4, 3, migration.Rollback)
	failed.Status, failed.RevertsExecutionID = migration.Failed, history[2].ID
	running := timelineExecution(5, 4, migration.Apply)
	running.Status = migration.Running
	history = append(history, failed, running)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, history)
	if err != nil || len(state.Active) != 3 || len(state.PendingForward) != 0 || state.BaseExecutionID != executionID(3) {
		t.Fatalf("attempt gagal/running mengubah posisi: %+v, %v", state, err)
	}
}

func TestPlanRollbackUsesLastActiveExecutionAndOriginalRevision(t *testing.T) {
	history := rolledBackTimeline()[:3]
	definition := timelineDefinition(history[2])
	definition.CurrentRevision = 9
	plan, err := migration.PlanAction(repositoryDatabaseID, history, definition, timelineRevision(history[2]), migration.Rollback)
	if err != nil || plan.Revision != 1 || plan.SQL != validRevision().SQLDown || plan.RevertsExecutionID != history[2].ID || plan.BaseExecutionID != history[2].ID {
		t.Fatalf("rencana rollback tidak sesuai: %+v, %v", plan, err)
	}
	_, err = migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(history[1]), timelineRevision(history[1]), migration.Rollback)
	if !errors.Is(err, migration.ErrConflict) {
		t.Fatalf("rollback migration sebelum posisi terakhir harus ditolak: %v", err)
	}
	revision := timelineRevision(history[2])
	revision.Number = 9
	if _, err := migration.PlanAction(repositoryDatabaseID, history, definition, revision, migration.Rollback); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("rollback menerima revisi pengganti:", err)
	}
	revision = timelineRevision(history[2])
	revision.SQLUp += "\n-- SQL berubah"
	if _, err := migration.PlanAction(repositoryDatabaseID, history, definition, revision, migration.Rollback); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("rollback menerima SQL up yang berbeda dari snapshot:", err)
	}
}

func TestPlanForwardFollowsRollbackOrderOneMigrationAtATime(t *testing.T) {
	history := rolledBackTimeline()
	b, c := history[1], history[2]
	if _, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(c), timelineRevision(c), migration.Forward); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("forward melewati migration B:", err)
	}
	plan, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(b), timelineRevision(b), migration.Forward)
	if err != nil || plan.MigrationID != b.MigrationID || plan.SQL != validRevision().SQLUp || plan.RevertsExecutionID != "" || plan.BaseExecutionID != history[4].ID {
		t.Fatalf("rencana forward tidak sesuai: %+v, %v", plan, err)
	}
	forwardB := timelineExecution(6, 2, migration.Forward)
	history = append(history, forwardB)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, history)
	if err != nil || len(state.Active) != 2 || len(state.PendingForward) != 1 || state.PendingForward[0].ID != c.ID {
		t.Fatalf("forward B otomatis mengubah migration lain: %+v, %v", state, err)
	}
	rollback, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(b), timelineRevision(b), migration.Rollback)
	if err != nil || rollback.RevertsExecutionID != forwardB.ID {
		t.Fatalf("rollback harus merujuk execution forward terbaru: %+v, %v", rollback, err)
	}
	revision := timelineRevision(c)
	revision.SQLUp += "\n-- SQL berubah"
	if _, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(c), revision, migration.Forward); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("forward menerima penggantian SQL asli:", err)
	}
}

func TestPlanApplyAndNewContinuationAfterRollback(t *testing.T) {
	history := rolledBackTimeline()
	b := history[1]
	if _, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(b), timelineRevision(b), migration.Apply); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("migration lama diterapkan ulang sebagai apply:", err)
	}
	d := timelineExecution(6, 4, migration.Apply)
	plan, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(d), timelineRevision(d), migration.Apply)
	if err != nil || plan.SQL != validRevision().SQLUp || plan.BaseExecutionID != history[4].ID {
		t.Fatalf("rencana apply kelanjutan baru tidak sesuai: %+v, %v", plan, err)
	}
	history = append(history, d)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, history)
	if err != nil || len(state.PendingForward) != 0 || len(state.Active) != 2 {
		t.Fatalf("kelanjutan baru tidak menutup jalur forward lama: %+v, %v", state, err)
	}
	if _, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(b), timelineRevision(b), migration.Forward); !errors.Is(err, migration.ErrConflict) {
		t.Fatal("forward kembali ke jalur lama diterima:", err)
	}
}

func TestPlanRejectsIrreversibleAndLegacyRollback(t *testing.T) {
	execution := timelineExecution(1, 1, migration.Apply)
	revision := timelineRevision(execution)
	revision.Irreversible, revision.SQLDown, revision.IrreversibleReason = true, "", "data asli tidak tersedia"
	if _, err := migration.PlanAction(repositoryDatabaseID, []migration.Execution{execution}, timelineDefinition(execution), revision, migration.Rollback); !errors.Is(err, migration.ErrIrreversibleMigration) {
		t.Fatal("rollback irreversible diterima:", err)
	}
	legacy := execution
	legacy.MigrationID, legacy.Revision = "", 0
	version := int64(1)
	legacy.LegacyVersion = &version
	if _, err := migration.PlanAction(repositoryDatabaseID, []migration.Execution{legacy}, timelineDefinition(execution), timelineRevision(execution), migration.Rollback); !errors.Is(err, migration.ErrIrreversibleMigration) {
		t.Fatal("rollback legacy tanpa SQL down diterima:", err)
	}
}

func TestBuildMigrationStateRejectsInconsistentHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]migration.Execution) []migration.Execution
	}{
		{"invalid database", func(h []migration.Execution) []migration.Execution { h[0].DatabaseID = "bad"; return h }},
		{"invalid execution ID", func(h []migration.Execution) []migration.Execution { h[0].ID = "bad"; return h }},
		{"duplicate execution ID", func(h []migration.Execution) []migration.Execution { h[1].ID = h[0].ID; return h }},
		{"duplicate sequence", func(h []migration.Execution) []migration.Execution { h[1].Sequence = h[0].Sequence; return h }},
		{"zero sequence", func(h []migration.Execution) []migration.Execution { h[0].Sequence = 0; return h }},
		{"invalid migration ID", func(h []migration.Execution) []migration.Execution { h[0].MigrationID = "bad"; return h }},
		{"invalid revision", func(h []migration.Execution) []migration.Execution { h[0].Revision = 0; return h }},
		{"repeated apply", func(h []migration.Execution) []migration.Execution { h[1].MigrationID = h[0].MigrationID; return h }},
		{"rollback out of order", func(h []migration.Execution) []migration.Execution {
			h[3].MigrationID, h[3].RevertsExecutionID = h[0].MigrationID, h[0].ID
			return h
		}},
		{"missing rollback reference", func(h []migration.Execution) []migration.Execution { h[3].RevertsExecutionID = ""; return h }},
		{"forward without rollback", func(h []migration.Execution) []migration.Execution { h[1].Action = migration.Forward; return h }},
		{"unknown action", func(h []migration.Execution) []migration.Execution { h[0].Action = "retry"; return h }},
		{"unknown status", func(h []migration.Execution) []migration.Execution { h[0].Status = ""; return h }},
		{"legacy with revision", func(h []migration.Execution) []migration.Execution {
			version := int64(1)
			h[0].LegacyVersion = &version
			return h
		}},
		{"forward out of order", func(h []migration.Execution) []migration.Execution {
			return append(h, timelineExecution(6, 3, migration.Forward))
		}},
		{"forward changed SQL", func(h []migration.Execution) []migration.Execution {
			e := timelineExecution(6, 2, migration.Forward)
			e.SQL += "changed"
			return append(h, e)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := tc.change(rolledBackTimeline())
			if _, err := migration.BuildMigrationState(repositoryDatabaseID, history); !errors.Is(err, migration.ErrInvalidHistory) {
				t.Fatalf("history tidak konsisten diterima: %v", err)
			}
		})
	}
}

func TestPlanActionValidatesEmptyAndScopedRequests(t *testing.T) {
	execution := timelineExecution(1, 1, migration.Apply)
	definition, revision := timelineDefinition(execution), timelineRevision(execution)
	if _, err := migration.PlanAction(repositoryDatabaseID, nil, definition, revision, migration.Apply); err != nil {
		t.Fatal("apply pertama ditolak:", err)
	}
	for _, action := range []migration.Action{migration.Rollback, migration.Forward} {
		if _, err := migration.PlanAction(repositoryDatabaseID, nil, definition, revision, action); !errors.Is(err, migration.ErrConflict) {
			t.Fatalf("tindakan %q diterima pada history kosong: %v", action, err)
		}
	}
	if _, err := migration.PlanAction(repositoryDatabaseID, nil, definition, revision, "bad"); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
	definition.DatabaseID = executionID(99)
	if _, err := migration.PlanAction(repositoryDatabaseID, nil, definition, revision, migration.Apply); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal("definisi database lain diterima:", err)
	}
	if _, err := migration.BuildMigrationState("bad", nil); !errors.Is(err, migration.ErrInvalidMigration) {
		t.Fatal(err)
	}
}

func TestForwardAfterRollingBackAllMigrations(t *testing.T) {
	history := rolledBackTimeline()
	rollbackA := timelineExecution(6, 1, migration.Rollback)
	rollbackA.RevertsExecutionID = history[0].ID
	history = append(history, rollbackA)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, history)
	if err != nil || len(state.Active) != 0 || len(state.PendingForward) != 3 {
		t.Fatalf("posisi setelah seluruh rollback tidak sesuai: %+v, %v", state, err)
	}
	a := history[0]
	plan, err := migration.PlanAction(repositoryDatabaseID, history, timelineDefinition(a), timelineRevision(a), migration.Forward)
	if err != nil || plan.MigrationID != a.MigrationID || plan.SQL != validRevision().SQLUp {
		t.Fatalf("forward pertama harus kembali ke migration A: %+v, %v", plan, err)
	}
}

func TestMigrationStateRequiresUnfilteredHistory(t *testing.T) {
	history := append(rolledBackTimeline(), timelineExecution(6, 2, migration.Forward))
	if _, err := migration.BuildMigrationState(repositoryDatabaseID, migration.HideRollback(history)); !errors.Is(err, migration.ErrInvalidHistory) {
		t.Fatal("history yang sudah difilter dipakai untuk menentukan posisi:", err)
	}
}

func TestMigrationStateDeduplicatesLegacyImportsAcrossAliases(t *testing.T) {
	version := int64(1)
	first := timelineExecution(1, 1, migration.Apply)
	first.MigrationID, first.Revision, first.LegacyVersion = "", 0, &version
	duplicate := first
	duplicate.ID, duplicate.Sequence, duplicate.DatabaseID = executionID(2), 2, executionID(99)
	state, err := migration.BuildMigrationState(repositoryDatabaseID, []migration.Execution{first, duplicate})
	if err != nil || len(state.Active) != 1 || state.BaseExecutionID != first.ID {
		t.Fatalf("impor legacy dihitung berulang: %+v, %v", state, err)
	}
	duplicate.SQL = "SELECT 999"
	if _, err := migration.BuildMigrationState(repositoryDatabaseID, []migration.Execution{first, duplicate}); !errors.Is(err, migration.ErrInvalidHistory) {
		t.Fatal("salinan legacy berbeda diterima", err)
	}
}

func TestPlanActionCarriesBatchCompensationSQL(t *testing.T) {
	execution := timelineExecution(1, 1, migration.Apply)
	revision := batchRevision()
	revision.MigrationID, revision.Number = execution.MigrationID, execution.Revision
	revision, err := migration.NormalizeRevision(revision)
	if err != nil {
		t.Fatal(err)
	}
	apply, err := migration.PlanAction(repositoryDatabaseID, nil, timelineDefinition(execution), revision, migration.Apply)
	if err != nil {
		t.Fatal(err)
	}
	if apply.CompensationSQL != revision.SQLDown || apply.SQL != revision.SQLUp || apply.Batch != revision.Batch {
		t.Fatalf("batch apply plan = %+v", apply)
	}

	execution.SQL = revision.SQLUp
	rollback, err := migration.PlanAction(repositoryDatabaseID, []migration.Execution{execution}, timelineDefinition(execution), revision, migration.Rollback)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.CompensationSQL != "" || rollback.SQL != revision.SQLDown || rollback.RevertsExecutionID != execution.ID {
		t.Fatalf("batch rollback plan = %+v", rollback)
	}

	regular, err := migration.PlanAction(repositoryDatabaseID, nil, timelineDefinition(execution), timelineRevision(execution), migration.Apply)
	if err != nil || regular.CompensationSQL != "" {
		t.Fatalf("regular plan = %+v, %v", regular, err)
	}
}
