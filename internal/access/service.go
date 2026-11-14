package access

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

//go:embed schema.sql
var schema embed.FS

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("conflict")
	ErrInvalidInput    = errors.New("invalid input")
)

type Service struct {
	db  *sql.DB
	now func() time.Time
}

type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	PlatformAdmin bool      `json:"platform_admin"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
}

type Group struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type GroupMember struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Active   bool   `json:"active"`
}

func NewService(db *sql.DB) *Service { return &Service{db: db, now: time.Now} }

// Initialize creates the access-control schema and creates the first admin only
// when the user table is empty. Later startup does not reset existing accounts.
func (s *Service) Initialize(ctx context.Context, bootstrapUsername, bootstrapPassword string) error {
	ddl, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read access schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(ddl)); err != nil {
		return fmt.Errorf("initialize access schema: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bootstrap transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9142027)`); err != nil {
		return fmt.Errorf("lock bootstrap admin creation: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM access_users").Scan(&count); err != nil {
		return fmt.Errorf("check bootstrap admin: %w", err)
	}
	if count != 0 {
		return tx.Commit()
	}
	username, err := normalizeUsername(bootstrapUsername)
	if err != nil || bootstrapPassword == "" {
		return errors.New("first startup requires BOOTSTRAP_ADMIN_USERNAME and BOOTSTRAP_ADMIN_PASSWORD")
	}
	if err := validatePassword(bootstrapPassword); err != nil {
		return fmt.Errorf("invalid bootstrap admin password: %w", err)
	}
	hash, err := hashPassword(bootstrapPassword)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_users (username, password_hash, is_platform_admin) VALUES ($1, $2, TRUE)`, username, hash); err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}
	return tx.Commit()
}

func (s *Service) CreateUser(ctx context.Context, username, password string, platformAdmin bool) (createdUser User, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "user_create", slog.Bool("platform_admin", platformAdmin))
	defer activity.End(&activityErr)

	username, err := normalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	if err := validatePassword(password); err != nil {
		return User{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.db.QueryRowContext(ctx, `INSERT INTO access_users (username, password_hash, is_platform_admin) VALUES ($1, $2, $3) RETURNING id::text, username, is_platform_admin, is_active, created_at`, username, hash, platformAdmin).
		Scan(&user.ID, &user.Username, &user.PlatformAdmin, &user.Active, &user.CreatedAt)
	if isUniqueViolation(err) {
		return User{}, ErrConflict
	}
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	activity.SetAttributes(slog.String("user_id", user.ID))
	return user, nil
}

func (s *Service) ListUsers(ctx context.Context) (result []User, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "users_list")
	defer activity.End(&activityErr)

	rows, err := s.db.QueryContext(ctx, `SELECT id::text, username, is_platform_admin, is_active, created_at FROM access_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Username, &user.PlatformAdmin, &user.Active, &user.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

// UpdateUser keeps at least one active Platform Admin. Reduced privileges
// revoke the user's sessions; increased privileges rotate them.
func (s *Service) UpdateUser(ctx context.Context, id string, active, platformAdmin bool) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "user_update", slog.String("user_id", id), slog.Bool("active", active), slog.Bool("platform_admin", platformAdmin))
	defer activity.End(&activityErr)

	if !validUUID(id) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(9142026)`); err != nil {
		return fmt.Errorf("lock platform admin update: %w", err)
	}
	var wasActive, wasAdmin bool
	err = tx.QueryRowContext(ctx, `SELECT is_active, is_platform_admin FROM access_users WHERE id = $1 FOR UPDATE`, id).Scan(&wasActive, &wasAdmin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load user before update: %w", err)
	}
	if wasActive && wasAdmin && (!active || !platformAdmin) {
		var activeAdmins int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_users WHERE is_active AND is_platform_admin`).Scan(&activeAdmins); err != nil {
			return fmt.Errorf("count platform admins: %w", err)
		}
		if activeAdmins <= 1 {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_users SET is_active = $1, is_platform_admin = $2 WHERE id = $3`, active, platformAdmin, id); err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	switch {
	case (wasActive && !active) || (wasAdmin && !platformAdmin):
		err = revokeSessions(ctx, tx, userSessions, id)
	case (!wasActive && active) || (!wasAdmin && platformAdmin):
		err = requireSessionRotation(ctx, tx, userSessions, id)
	}
	if err != nil {
		return fmt.Errorf("update sessions after user change: %w", err)
	}
	return tx.Commit()
}

func (s *Service) CreateGroup(ctx context.Context, name string) (createdGroup Group, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_create")
	defer activity.End(&activityErr)

	name, err := normalizeGroupName(name)
	if err != nil {
		return Group{}, err
	}
	var group Group
	err = s.db.QueryRowContext(ctx, `INSERT INTO access_groups (name) VALUES ($1) RETURNING id::text, name, created_at`, name).Scan(&group.ID, &group.Name, &group.CreatedAt)
	if isUniqueViolation(err) {
		return Group{}, ErrConflict
	}
	if err != nil {
		return Group{}, fmt.Errorf("create group: %w", err)
	}
	activity.SetAttributes(slog.String("group_id", group.ID))
	return group, nil
}

func (s *Service) ListGroups(ctx context.Context) (result []Group, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "groups_list")
	defer activity.End(&activityErr)

	rows, err := s.db.QueryContext(ctx, `SELECT id::text, name, created_at FROM access_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name, &group.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Service) ListGroupMembers(ctx context.Context, groupID string) (result []GroupMember, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_members_list", slog.String("group_id", groupID))
	defer activity.End(&activityErr)

	if !validUUID(groupID) {
		return nil, ErrNotFound
	}
	if err := s.requireGroup(ctx, groupID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id::text, u.username, u.is_active FROM access_group_members m JOIN access_users u ON u.id = m.user_id WHERE m.group_id = $1 ORDER BY u.username`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	members := make([]GroupMember, 0)
	for rows.Next() {
		var member GroupMember
		if err := rows.Scan(&member.ID, &member.Username, &member.Active); err != nil {
			return nil, fmt.Errorf("scan group member: %w", err)
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func (s *Service) DeleteGroup(ctx context.Context, groupID string) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_delete", slog.String("group_id", groupID))
	defer activity.End(&activityErr)

	if !validUUID(groupID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := revokeSessions(ctx, tx, groupSessions, groupID); err != nil {
		return fmt.Errorf("revoke group sessions: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM access_groups WHERE id = $1`, groupID)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	return tx.Commit()
}

// SetMembership adds or removes a group member. Joining rotates the member's
// sessions; leaving revokes them because the member may lose database access.
func (s *Service) SetMembership(ctx context.Context, groupID, userID string, member bool) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "group_membership_set", slog.String("group_id", groupID), slog.String("user_id", userID), slog.Bool("member", member))
	defer activity.End(&activityErr)

	if !validUUID(groupID) || !validUUID(userID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if member {
		err = addMember(ctx, tx, groupID, userID)
	} else {
		err = removeMember(ctx, tx, groupID, userID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func addMember(ctx context.Context, tx *sql.Tx, groupID, userID string) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO access_group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID)
	if err != nil {
		return mapMissingOrError(err)
	}
	added, err := result.RowsAffected()
	if err != nil || added == 0 {
		return err
	}
	if err := requireSessionRotation(ctx, tx, userSessions, userID); err != nil {
		return fmt.Errorf("rotate sessions after group membership grant: %w", err)
	}
	return nil
}

func removeMember(ctx context.Context, tx *sql.Tx, groupID, userID string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM access_group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	if err := requireAffected(result); err != nil {
		return err
	}
	if err := revokeSessions(ctx, tx, userSessions, userID); err != nil {
		return fmt.Errorf("revoke sessions after group membership removal: %w", err)
	}
	return nil
}

func (s *Service) requireGroup(ctx context.Context, groupID string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM access_groups WHERE id = $1)`, groupID).Scan(&exists); err != nil {
		return fmt.Errorf("check group: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func requireAffected(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
