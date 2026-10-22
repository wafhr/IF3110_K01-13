package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed schema.sql
var schema embed.FS

const (
	idleTimeout      = 30 * time.Minute
	absoluteLifetime = 12 * time.Hour
	rotationInterval = 15 * time.Minute
	rotationGrace    = 30 * time.Second
)

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

type DatabaseGrant struct {
	DatabaseID string     `json:"database_id"`
	Permission Permission `json:"permission"`
}

type Permission string

const (
	ViewOnly Permission = "view_only"
	ViewEdit Permission = "view_edit"
)

type Session struct {
	User           User
	CSRFToken      string
	RotatedToken   string
	AbsoluteExpiry time.Time
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

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	username, err := normalizeUsername(username)
	if err != nil || password == "" {
		// Keep the work factor similar for malformed and unknown accounts.
		_, _ = hashPassword("invalid-login-timing-equalizer")
		return Session{}, ErrUnauthenticated
	}
	var user User
	var encoded string
	err = s.db.QueryRowContext(ctx, `SELECT id::text, username, password_hash, is_platform_admin, is_active, created_at FROM access_users WHERE username = $1`, username).
		Scan(&user.ID, &user.Username, &encoded, &user.PlatformAdmin, &user.Active, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		// The fixed hash still uses the configured PBKDF2 work factor.
		_, _ = hashPassword("invalid-login-timing-equalizer")
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("load user for login: %w", err)
	}
	if !verifyPassword(encoded, password) || !user.Active {
		return Session{}, ErrUnauthenticated
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE access_sessions SET current_token_during_grace = NULL, current_token_grace_expires_at = NULL WHERE current_token_grace_expires_at <= $1`, now); err != nil {
		return Session{}, fmt.Errorf("clean expired rotation tokens: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM access_sessions WHERE absolute_expires_at <= $1 OR last_seen_at <= $2`, now, now.Add(-idleTimeout)); err != nil {
		return Session{}, fmt.Errorf("clean expired sessions: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO access_sessions (token_hash, csrf_token, user_id, created_at, last_seen_at, rotated_at, absolute_expires_at) VALUES ($1, $2, $3, $4, $4, $4, $5)`, tokenHash(token), csrf, user.ID, now, now.Add(absoluteLifetime)); err != nil {
		return Session{}, fmt.Errorf("create login session: %w", err)
	}
	return Session{User: user, CSRFToken: csrf, RotatedToken: token, AbsoluteExpiry: now.Add(absoluteLifetime)}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrUnauthenticated
	}
	now := s.now().UTC()
	hash := tokenHash(token)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin session check: %w", err)
	}
	defer tx.Rollback()
	var session Session
	var currentHash []byte
	var graceToken sql.NullString
	var graceExpiry sql.NullTime
	var lastSeen, rotatedAt time.Time
	var isCurrent bool
	err = tx.QueryRowContext(ctx, `
		SELECT s.token_hash, (s.token_hash = $1), s.current_token_during_grace, s.current_token_grace_expires_at, s.csrf_token,
		       s.last_seen_at, s.rotated_at, s.absolute_expires_at,
		       u.id::text, u.username, u.is_platform_admin, u.is_active, u.created_at
		FROM access_sessions s JOIN access_users u ON u.id = s.user_id
		WHERE s.token_hash = $1 OR (s.previous_token_hash = $1 AND s.previous_expires_at > $2)
		FOR UPDATE OF s`, hash, now).Scan(&currentHash, &isCurrent, &graceToken, &graceExpiry, &session.CSRFToken,
		&lastSeen, &rotatedAt, &session.AbsoluteExpiry, &session.User.ID, &session.User.Username,
		&session.User.PlatformAdmin, &session.User.Active, &session.User.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	if !session.User.Active || !now.Before(session.AbsoluteExpiry) || !now.Before(lastSeen.Add(idleTimeout)) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE token_hash = $1`, currentHash); err != nil {
			return Session{}, fmt.Errorf("expire session: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit expired session: %w", err)
		}
		return Session{}, ErrUnauthenticated
	}
	if isCurrent && !now.Before(rotatedAt.Add(rotationInterval)) {
		next, err := randomToken()
		if err != nil {
			return Session{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE access_sessions SET previous_token_hash = token_hash, previous_expires_at = $1, token_hash = $2, current_token_during_grace = $3, current_token_grace_expires_at = $1, last_seen_at = $4, rotated_at = $4 WHERE token_hash = $5`, now.Add(rotationGrace), tokenHash(next), next, now, currentHash)
		if err != nil {
			return Session{}, fmt.Errorf("rotate session: %w", err)
		}
		session.RotatedToken = next
	} else {
		if !isCurrent && graceToken.Valid && graceExpiry.Valid && now.Before(graceExpiry.Time) {
			session.RotatedToken = graceToken.String
		}
		_, err = tx.ExecContext(ctx, `UPDATE access_sessions SET last_seen_at = $1, current_token_during_grace = CASE WHEN current_token_grace_expires_at <= $1 THEN NULL ELSE current_token_during_grace END, current_token_grace_expires_at = CASE WHEN current_token_grace_expires_at <= $1 THEN NULL ELSE current_token_grace_expires_at END WHERE token_hash = $2`, now, currentHash)
		if err != nil {
			return Session{}, fmt.Errorf("refresh session activity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session check: %w", err)
	}
	return session, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM access_sessions WHERE token_hash = $1 OR previous_token_hash = $1`, tokenHash(token)); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *Service) CreateUser(ctx context.Context, username, password string, platformAdmin bool) (User, error) {
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
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
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

func (s *Service) UpdateUser(ctx context.Context, id string, active, platformAdmin bool) error {
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
	if err := tx.QueryRowContext(ctx, `SELECT is_active, is_platform_admin FROM access_users WHERE id = $1 FOR UPDATE`, id).Scan(&wasActive, &wasAdmin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
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
	_, err = tx.ExecContext(ctx, `UPDATE access_users SET is_active = $1, is_platform_admin = $2 WHERE id = $3`, active, platformAdmin, id)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	reduced := (wasActive && !active) || (wasAdmin && !platformAdmin)
	increased := (!wasActive && active) || (!wasAdmin && platformAdmin)
	if reduced {
		if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE user_id = $1`, id); err != nil {
			return fmt.Errorf("revoke sessions after user update: %w", err)
		}
	} else if increased {
		if err := rotateSessions(ctx, tx, `SELECT token_hash FROM access_sessions WHERE user_id = $1 FOR UPDATE`, id, s.now().UTC()); err != nil {
			return fmt.Errorf("rotate sessions after privilege increase: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Service) CreateGroup(ctx context.Context, name string) (Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return Group{}, errors.New("group name must contain 1 to 100 characters")
	}
	var group Group
	err := s.db.QueryRowContext(ctx, `INSERT INTO access_groups (name) VALUES ($1) RETURNING id::text, name, created_at`, name).Scan(&group.ID, &group.Name, &group.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return Group{}, ErrConflict
		}
		return Group{}, fmt.Errorf("create group: %w", err)
	}
	return group, nil
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id::text, name, created_at FROM access_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name, &group.CreatedAt); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Service) ListGroupMembers(ctx context.Context, groupID string) ([]GroupMember, error) {
	if !validUUID(groupID) {
		return nil, ErrNotFound
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
			return nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM access_groups WHERE id = $1)`, groupID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	return members, nil
}

func (s *Service) ListDatabaseGrants(ctx context.Context, groupID string) ([]DatabaseGrant, error) {
	if !validUUID(groupID) {
		return nil, ErrNotFound
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
			return nil, err
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM access_groups WHERE id = $1)`, groupID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	return grants, nil
}

func (s *Service) DeleteGroup(ctx context.Context, groupID string) error {
	if !validUUID(groupID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE user_id IN (SELECT user_id FROM access_group_members WHERE group_id = $1)`, groupID); err != nil {
		return fmt.Errorf("revoke group sessions: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM access_groups WHERE id = $1`, groupID)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Service) SetMembership(ctx context.Context, groupID, userID string, member bool) error {
	if !validUUID(groupID) || !validUUID(userID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var result sql.Result
	if member {
		result, err = tx.ExecContext(ctx, `INSERT INTO access_group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID)
	} else {
		result, err = tx.ExecContext(ctx, `DELETE FROM access_group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID)
	}
	if err != nil {
		return mapMissingOrError(err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 0 {
		if member {
			if err := rotateSessions(ctx, tx, `SELECT token_hash FROM access_sessions WHERE user_id = $1 FOR UPDATE`, userID, s.now().UTC()); err != nil {
				return fmt.Errorf("rotate sessions after group membership grant: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("revoke sessions after group membership removal: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Service) SetDatabasePermission(ctx context.Context, groupID, databaseID string, permission Permission) error {
	if !validUUID(groupID) || !validUUID(databaseID) || (permission != ViewOnly && permission != ViewEdit) {
		return ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldPermission string
	err = tx.QueryRowContext(ctx, `SELECT permission FROM access_group_database_permissions WHERE group_id = $1 AND database_id = $2 FOR UPDATE`, groupID, databaseID).Scan(&oldPermission)
	grantExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load existing database permission: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_group_database_permissions (group_id, database_id, permission) VALUES ($1, $2, $3) ON CONFLICT (group_id, database_id) DO UPDATE SET permission = EXCLUDED.permission`, groupID, databaseID, string(permission))
	if err != nil {
		return mapMissingOrError(err)
	}
	if !grantExists || (oldPermission == string(ViewOnly) && permission == ViewEdit) {
		if err := rotateSessions(ctx, tx, `SELECT s.token_hash FROM access_sessions s WHERE s.user_id IN (SELECT user_id FROM access_group_members WHERE group_id = $1) FOR UPDATE`, groupID, s.now().UTC()); err != nil {
			return fmt.Errorf("rotate group sessions after permission grant: %w", err)
		}
	} else if oldPermission == string(ViewEdit) && permission == ViewOnly {
		if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE user_id IN (SELECT user_id FROM access_group_members WHERE group_id = $1)`, groupID); err != nil {
			return fmt.Errorf("revoke group sessions after permission downgrade: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Service) DeleteDatabasePermission(ctx context.Context, groupID, databaseID string) error {
	if !validUUID(groupID) || !validUUID(databaseID) {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `DELETE FROM access_group_database_permissions WHERE group_id = $1 AND database_id = $2`, groupID, databaseID)
	if err != nil {
		return fmt.Errorf("delete database permission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE user_id IN (SELECT user_id FROM access_group_members WHERE group_id = $1)`, groupID); err != nil {
		return fmt.Errorf("revoke group sessions after permission removal: %w", err)
	}
	return tx.Commit()
}

func rotateSessions(ctx context.Context, tx *sql.Tx, query, id string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, query, id)
	if err != nil {
		return err
	}
	var hashes [][]byte
	for rows.Next() {
		var hash []byte
		if err := rows.Scan(&hash); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, oldHash := range hashes {
		token, err := randomToken()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE access_sessions SET previous_token_hash = token_hash, previous_expires_at = $1, token_hash = $2, current_token_during_grace = $3, current_token_grace_expires_at = $1, rotated_at = $4, last_seen_at = $4 WHERE token_hash = $5`, now.Add(rotationGrace), tokenHash(token), token, now, oldHash); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) EffectivePermission(ctx context.Context, userID, databaseID string) (Permission, error) {
	if !validUUID(userID) || !validUUID(databaseID) {
		return "", ErrNotFound
	}
	var permission sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT CASE WHEN bool_or(p.permission = 'view_edit') THEN 'view_edit' ELSE 'view_only' END
		FROM access_group_members m
		JOIN access_group_database_permissions p ON p.group_id = m.group_id
		WHERE m.user_id = $1 AND p.database_id = $2
		HAVING count(*) > 0`, userID, databaseID).Scan(&permission)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve database permission: %w", err)
	}
	if !permission.Valid {
		return "", nil
	}
	return Permission(permission.String), nil
}

// AuthorizeDatabase applies group-derived permissions to an individual action.
// Platform-admin status intentionally does not grant database access.
func (s *Service) AuthorizeDatabase(ctx context.Context, userID, databaseID string, minimum Permission) error {
	if minimum != ViewOnly && minimum != ViewEdit {
		return ErrInvalidInput
	}
	effective, err := s.EffectivePermission(ctx, userID, databaseID)
	if err != nil {
		return err
	}
	if effective == "" || (minimum == ViewEdit && effective != ViewEdit) {
		return ErrForbidden
	}
	return nil
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate opaque token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func tokenHash(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}
