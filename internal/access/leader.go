package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/uuid"
)

var (
	errLeaderRequired = invalidInput("assign a group leader before granting database access")
	errLeaderNeeded   = invalidInput("a group with database access must keep a leader; assign another leader first")
)

// groupLeadership locks the group row and reports its leader and whether it
// holds any database grant.
func groupLeadership(ctx context.Context, tx *sql.Tx, groupID string) (leaderID string, hasGrants bool, err error) {
	var leader sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT leader_user_id::text, EXISTS (
		SELECT 1 FROM access_group_database_permissions WHERE group_id = $1
	) FROM access_groups WHERE id = $1 FOR UPDATE`, groupID).Scan(&leader, &hasGrants)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("lock group leadership: %w", err)
	}
	return leader.String, hasGrants, nil
}

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
// empty userID clears the assignment, which is refused while the group holds a
// database grant. Membership and assignment are serialized by locking the group
// row before validation.
func (s *Service) SetGroupLeader(ctx context.Context, groupID, userID string) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_leader_set", slog.String("group_id", groupID), slog.String("user_id", userID))
	defer activity.End(&activityErr)
	if !uuid.Valid(groupID) || (userID != "" && !uuid.Valid(userID)) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin leader assignment: %w", err)
	}
	defer tx.Rollback()
	_, hasGrants, err := groupLeadership(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if userID == "" && hasGrants {
		return errLeaderNeeded
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
