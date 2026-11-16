package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func bumpGroupApprovalEpochs(ctx context.Context, tx *sql.Tx, groupID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO access_database_approval_epochs(database_id, epoch)
		SELECT DISTINCT database_id, 2 FROM access_group_database_permissions WHERE group_id = $1
		ON CONFLICT(database_id) DO UPDATE SET epoch = access_database_approval_epochs.epoch + 1`, groupID)
	return err
}

func bumpDatabaseApprovalEpoch(ctx context.Context, tx *sql.Tx, databaseID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO access_database_approval_epochs(database_id, epoch) VALUES ($1, 2)
		ON CONFLICT(database_id) DO UPDATE SET epoch = access_database_approval_epochs.epoch + 1`, databaseID)
	return err
}

func bumpLeaderApprovalEpochs(ctx context.Context, tx *sql.Tx, userID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO access_database_approval_epochs(database_id, epoch)
		SELECT DISTINCT p.database_id, 2 FROM access_groups g
		JOIN access_group_database_permissions p ON p.group_id = g.id WHERE g.leader_user_id = $1
		ON CONFLICT(database_id) DO UPDATE SET epoch = access_database_approval_epochs.epoch + 1`, userID)
	return err
}

// SetGroupLeader assigns an active member as the group's single leader. An
// empty userID clears the assignment. Membership and assignment are serialized
// by locking the group row before validation.
func (s *Service) SetGroupLeader(ctx context.Context, groupID, userID string) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_leader_set", slog.String("group_id", groupID), slog.String("user_id", userID))
	defer activity.End(&activityErr)
	if !validUUID(groupID) || (userID != "" && !validUUID(userID)) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin leader assignment: %w", err)
	}
	defer tx.Rollback()
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT id::text FROM access_groups WHERE id = $1 FOR UPDATE`, groupID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock group for leader assignment: %w", err)
	}
	if userID != "" {
		var eligible bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM access_group_members m JOIN access_users u ON u.id = m.user_id
			WHERE m.group_id = $1 AND m.user_id = $2 AND u.is_active
		)`, groupID, userID).Scan(&eligible)
		if err != nil {
			return fmt.Errorf("validate group leader: %w", err)
		}
		if !eligible {
			return ErrInvalidInput
		}
	}
	if err := bumpGroupApprovalEpochs(ctx, tx, groupID); err != nil {
		return fmt.Errorf("invalidate approvals after leader change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_groups SET leader_user_id = NULLIF($2, '')::uuid WHERE id = $1`, groupID, userID); err != nil {
		return mapMissingOrError(err)
	}
	return tx.Commit()
}
