package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

type Permission string

const (
	ViewOnly Permission = "view_only"
	ViewEdit Permission = "view_edit"
)

type DatabaseGrant struct {
	DatabaseID string     `json:"database_id"`
	Permission Permission `json:"permission"`
}

func (p Permission) rank() int {
	switch p {
	case ViewOnly:
		return 1
	case ViewEdit:
		return 2
	default:
		return 0
	}
}

func (p Permission) valid() bool { return p.rank() > 0 }

func (p Permission) satisfies(minimum Permission) bool {
	return minimum.valid() && p.rank() >= minimum.rank()
}

func (s *Service) ListDatabaseGrants(ctx context.Context, groupID string) (result []DatabaseGrant, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "database_permissions_list", slog.String("group_id", groupID))
	defer activity.End(&activityErr)

	if !validUUID(groupID) {
		return nil, ErrNotFound
	}
	if err := s.requireGroup(ctx, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT database_id::text, permission FROM access_group_database_permissions WHERE group_id = $1 ORDER BY database_id`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group database grants: %w", err)
	}
	defer rows.Close()
	grants := make([]DatabaseGrant, 0)
	for rows.Next() {
		var grant DatabaseGrant
		if err := rows.Scan(&grant.DatabaseID, &grant.Permission); err != nil {
			return nil, fmt.Errorf("scan database grant: %w", err)
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// SetDatabasePermission rotates member sessions when access grows and revokes
// them when access shrinks, so every session reflects the current grant.
func (s *Service) SetDatabasePermission(ctx context.Context, groupID, databaseID string, permission Permission) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "database_permission_set", slog.String("group_id", groupID), slog.String("database_id", databaseID))
	defer activity.End(&activityErr)

	if !validUUID(groupID) || !validUUID(databaseID) {
		return ErrNotFound
	}
	if !permission.valid() {
		return invalidInput("permission must be view_only or view_edit")
	}
	activity.SetAttributes(slog.String("permission", string(permission)))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep the registration alive until the grant commits. Admin deletion locks
	// this same row before removing grants and revoking the affected sessions.
	var registeredID string
	err = tx.QueryRowContext(ctx, `SELECT id::text FROM managed_databases WHERE id = $1 FOR KEY SHARE`, databaseID).Scan(&registeredID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check registered database before grant: %w", err)
	}
	var previous Permission
	err = tx.QueryRowContext(ctx, `SELECT permission FROM access_group_database_permissions WHERE group_id = $1 AND database_id = $2 FOR UPDATE`, groupID, databaseID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load existing database permission: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_group_database_permissions (group_id, database_id, permission) VALUES ($1, $2, $3) ON CONFLICT (group_id, database_id) DO UPDATE SET permission = EXCLUDED.permission`, groupID, databaseID, string(permission))
	if err != nil {
		return mapMissingOrError(err)
	}
	switch {
	case permission.rank() > previous.rank():
		err = requireSessionRotation(ctx, tx, groupSessions, groupID)
	case permission.rank() < previous.rank():
		err = revokeSessions(ctx, tx, groupSessions, groupID)
	}
	if err != nil {
		return fmt.Errorf("update group sessions after permission change: %w", err)
	}
	return tx.Commit()
}

func (s *Service) DeleteDatabasePermission(ctx context.Context, groupID, databaseID string) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "database_permission_delete", slog.String("group_id", groupID), slog.String("database_id", databaseID))
	defer activity.End(&activityErr)

	if !validUUID(groupID) || !validUUID(databaseID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM access_group_database_permissions WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	if err != nil {
		return fmt.Errorf("delete database permission: %w", err)
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	if err := revokeSessions(ctx, tx, groupSessions, groupID); err != nil {
		return fmt.Errorf("revoke group sessions after permission removal: %w", err)
	}
	return tx.Commit()
}

// EffectivePermission returns the strongest grant among the active user's
// groups, or an empty permission when no group grants the database.
func (s *Service) EffectivePermission(ctx context.Context, userID, databaseID string) (Permission, error) {
	if !validUUID(userID) || !validUUID(databaseID) {
		return "", ErrNotFound
	}
	var permission sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN bool_or(p.permission = 'view_edit') THEN 'view_edit' ELSE 'view_only' END
		FROM access_group_members m
		JOIN access_users u ON u.id = m.user_id AND u.is_active
		JOIN access_group_database_permissions p ON p.group_id = m.group_id
		WHERE m.user_id = $1 AND p.database_id = $2
		HAVING count(*) > 0`, userID, databaseID).Scan(&permission)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve database permission: %w", err)
	}
	return Permission(permission.String), nil
}

// AuthorizeDatabase applies group-derived permissions to an individual action.
// Platform-admin status does not grant database access.
func (s *Service) AuthorizeDatabase(ctx context.Context, userID, databaseID string, minimum Permission) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "database_authorize", slog.String("actor", userID), slog.String("database_id", databaseID))
	defer activity.End(&activityErr)

	if !minimum.valid() {
		return ErrInvalidInput
	}
	effective, err := s.EffectivePermission(ctx, userID, databaseID)
	if err != nil {
		return err
	}
	if !effective.satisfies(minimum) {
		return ErrForbidden
	}
	return nil
}
