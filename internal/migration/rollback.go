package migration

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// MigrationState menunjukkan posisi aktif berdasarkan history commit lengkap.
// Active berurutan dari awal ke akhir. Elemen terakhir PendingForward adalah
// migration berikutnya yang dapat diterapkan kembali.
type MigrationState struct {
	DatabaseID        string
	BaseExecutionID   string
	Active            []Execution
	PendingForward    []Execution
	appliedMigrations map[string]bool
}

// BuildMigrationState menerima history lengkap, termasuk pasangan rollback yang
// disembunyikan UI dan commit lewat alias registrasi pada target fisik yang sama.
// Attempt gagal/running tidak mengubah posisi migration.
func BuildMigrationState(databaseID string, history []Execution) (MigrationState, error) {
	if !validUUID(databaseID) {
		return MigrationState{}, ErrInvalidMigration
	}
	committed := make([]Execution, 0, len(history))
	for _, execution := range history {
		switch execution.Status {
		case Success:
			committed = append(committed, execution)
		case Failed, Running:
			continue
		default:
			return MigrationState{}, ErrInvalidHistory
		}
	}
	slices.SortFunc(committed, func(left, right Execution) int { return cmp.Compare(left.Sequence, right.Sequence) })
	state := MigrationState{
		DatabaseID: databaseID, Active: make([]Execution, 0), PendingForward: make([]Execution, 0),
		appliedMigrations: make(map[string]bool),
	}
	seenExecutions := make(map[string]bool, len(committed))
	seenLegacy := make(map[int64]Execution)
	var previousSequence int64
	for _, execution := range committed {
		key := strings.ToLower(execution.ID)
		if !validUUID(execution.ID) || !validUUID(execution.DatabaseID) ||
			execution.Sequence <= previousSequence || seenExecutions[key] {
			return MigrationState{}, ErrInvalidHistory
		}
		if execution.LegacyVersion != nil {
			if *execution.LegacyVersion < 1 || execution.MigrationID != "" || execution.Revision != 0 || execution.Action != Apply {
				return MigrationState{}, ErrInvalidHistory
			}
			// Impor lama melalui beberapa alias dapat menyalin versi legacy yang sama.
			if original, exists := seenLegacy[*execution.LegacyVersion]; exists {
				if execution.SQL != original.SQL || execution.Name != original.Name || execution.ExecutedBy != original.ExecutedBy {
					return MigrationState{}, ErrInvalidHistory
				}
				seenExecutions[key] = true
				previousSequence = execution.Sequence
				continue
			}
			seenLegacy[*execution.LegacyVersion] = execution
		} else if !validUUID(execution.MigrationID) || execution.Revision < 1 {
			return MigrationState{}, ErrInvalidHistory
		}
		switch execution.Action {
		case Apply:
			migrationKey := strings.ToLower(execution.MigrationID)
			if execution.RevertsExecutionID != "" || (migrationKey != "" && state.appliedMigrations[migrationKey]) {
				return MigrationState{}, ErrInvalidHistory
			}
			state.appliedMigrations[migrationKey] = true
			state.Active = append(state.Active, execution)
			// Apply baru menetapkan kelanjutan baru setelah rollback sebelumnya.
			state.PendingForward = state.PendingForward[:0]
		case Rollback:
			if len(state.Active) == 0 {
				return MigrationState{}, ErrInvalidHistory
			}
			original := state.Active[len(state.Active)-1]
			if !strings.EqualFold(execution.RevertsExecutionID, original.ID) ||
				!strings.EqualFold(execution.DatabaseID, original.DatabaseID) ||
				!strings.EqualFold(execution.MigrationID, original.MigrationID) || execution.Revision != original.Revision {
				return MigrationState{}, ErrInvalidHistory
			}
			state.Active = state.Active[:len(state.Active)-1]
			state.PendingForward = append(state.PendingForward, original)
		case Forward:
			if len(state.PendingForward) == 0 {
				return MigrationState{}, ErrInvalidHistory
			}
			original := state.PendingForward[len(state.PendingForward)-1]
			if execution.RevertsExecutionID != "" || !strings.EqualFold(execution.MigrationID, original.MigrationID) ||
				!strings.EqualFold(execution.DatabaseID, original.DatabaseID) ||
				execution.Revision != original.Revision || execution.SQL != original.SQL {
				return MigrationState{}, ErrInvalidHistory
			}
			state.PendingForward = state.PendingForward[:len(state.PendingForward)-1]
			state.Active = append(state.Active, execution)
		default:
			return MigrationState{}, ErrInvalidHistory
		}
		seenExecutions[key] = true
		previousSequence = execution.Sequence
		state.BaseExecutionID = execution.ID
	}
	return state, nil
}

// PlanAction memeriksa urutan dan memilih SQL dari revisi yang tersimpan.
// Satu rencana hanya memuat satu tindakan pada satu migration.
func PlanAction(databaseID string, history []Execution, definition Migration, revision Revision, action Action) (ActionPlan, error) {
	state, err := BuildMigrationState(databaseID, history)
	if err != nil {
		return ActionPlan{}, err
	}
	return state.planAction(definition, revision, action)
}

func (state MigrationState) actionRevision(definition Migration, action Action) (int64, string, error) {
	if !validUUID(definition.ID) || !strings.EqualFold(definition.DatabaseID, state.DatabaseID) {
		return 0, "", ErrInvalidMigration
	}
	switch action {
	case Apply:
		if state.appliedMigrations[strings.ToLower(definition.ID)] {
			return 0, "", fmt.Errorf("%w: an applied migration requires forward", ErrConflict)
		}
		if definition.CurrentRevision < 1 {
			return 0, "", ErrInvalidMigration
		}
		return definition.CurrentRevision, "", nil
	case Rollback:
		if len(state.Active) == 0 {
			return 0, "", fmt.Errorf("%w: no active migration to roll back", ErrConflict)
		}
		original := state.Active[len(state.Active)-1]
		if original.LegacyVersion != nil {
			return 0, "", ErrIrreversibleMigration
		}
		if !strings.EqualFold(original.MigrationID, definition.ID) {
			return 0, "", fmt.Errorf("%w: only the last active migration can be rolled back", ErrConflict)
		}
		return original.Revision, original.ID, nil
	case Forward:
		if len(state.PendingForward) == 0 {
			return 0, "", fmt.Errorf("%w: no migration awaiting forward", ErrConflict)
		}
		original := state.PendingForward[len(state.PendingForward)-1]
		if !strings.EqualFold(original.MigrationID, definition.ID) {
			return 0, "", fmt.Errorf("%w: forward must follow the original linear order", ErrConflict)
		}
		return original.Revision, "", nil
	default:
		return 0, "", ErrInvalidMigration
	}
}

func (state MigrationState) planAction(definition Migration, revision Revision, action Action) (ActionPlan, error) {
	number, revertsID, err := state.actionRevision(definition, action)
	if err != nil {
		return ActionPlan{}, err
	}
	if !strings.EqualFold(revision.MigrationID, definition.ID) || revision.Number != number {
		return ActionPlan{}, fmt.Errorf("%w: action must use its original revision", ErrConflict)
	}
	statement, err := revision.SQLFor(action)
	if err != nil {
		return ActionPlan{}, err
	}
	if action == Rollback && revision.SQLUp != state.Active[len(state.Active)-1].SQL {
		return ActionPlan{}, fmt.Errorf("%w: original SQL does not match execution history", ErrConflict)
	}
	if action == Forward && revision.SQLUp != state.PendingForward[len(state.PendingForward)-1].SQL {
		return ActionPlan{}, fmt.Errorf("%w: original SQL does not match execution history", ErrConflict)
	}
	plan := ActionPlan{
		DatabaseID: state.DatabaseID, MigrationID: definition.ID, Revision: number, Name: definition.Name,
		Action: action, SQL: statement, RevertsExecutionID: revertsID, BaseExecutionID: state.BaseExecutionID,
		Batch: revision.Batch,
	}
	// A batch apply or forward carries the revision's down SQL so cancelling its
	// committed batches runs SQL that was reviewed and approved with the plan.
	if revision.Batch.Enabled() && action != Rollback {
		plan.CompensationSQL = revision.SQLDown
	}
	return plan, nil
}
