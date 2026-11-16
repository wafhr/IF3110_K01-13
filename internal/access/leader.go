package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

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
	if _, err := tx.ExecContext(ctx, `UPDATE access_groups SET leader_user_id = NULLIF($2, '')::uuid WHERE id = $1`, groupID, userID); err != nil {
		return mapMissingOrError(err)
	}
	return tx.Commit()
}
