package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const (
	idleTimeout      = 30 * time.Minute
	absoluteLifetime = 12 * time.Hour
	rotationInterval = 15 * time.Minute
	rotationGrace    = 30 * time.Second
)

type Session struct {
	User           User
	CSRFToken      string
	RotatedToken   string
	AbsoluteExpiry time.Time
}

type sessionScope string

const (
	userSessions  sessionScope = "user_id = $1"
	groupSessions sessionScope = "user_id IN (SELECT user_id FROM access_group_members WHERE group_id = $1)"
)

type sessionRecord struct {
	session          Session
	currentHash      []byte
	presentedCurrent bool
	graceToken       sql.NullString
	graceExpiry      sql.NullTime
	lastSeen         time.Time
	rotatedAt        time.Time
	rotationRequired bool
}

func (s *Service) Login(ctx context.Context, username, password string) (loginSession Session, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "auth_login", slog.String("actor", "anonymous"))
	defer activity.End(&activityErr)

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
	ctx = telemetry.WithActor(ctx, user.ID)
	activity.SetAttributes(slog.String("actor", user.ID))
	now := s.now().UTC()
	if err := s.removeStaleSessions(ctx, now); err != nil {
		return Session{}, err
	}
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	expiry := now.Add(absoluteLifetime)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO access_sessions (token_hash, csrf_token, user_id, created_at, last_seen_at, rotated_at, absolute_expires_at) VALUES ($1, $2, $3, $4, $4, $4, $5)`, tokenHash(token), csrf, user.ID, now, expiry); err != nil {
		return Session{}, fmt.Errorf("create login session: %w", err)
	}
	return Session{User: user, CSRFToken: csrf, RotatedToken: token, AbsoluteExpiry: expiry}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (validatedSession Session, activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "session_validate", slog.String("actor", "anonymous"))
	defer activity.End(&activityErr)

	if token == "" {
		return Session{}, ErrUnauthenticated
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin session check: %w", err)
	}
	defer tx.Rollback()
	record, err := loadSession(ctx, tx, tokenHash(token), now)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, fmt.Errorf("load session: %w", err)
	}
	if record.expired(now) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE token_hash = $1`, record.currentHash); err != nil {
			return Session{}, fmt.Errorf("expire session: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit expired session: %w", err)
		}
		return Session{}, ErrUnauthenticated
	}
	session := record.session
	ctx = telemetry.WithActor(ctx, session.User.ID)
	activity.SetAttributes(slog.String("actor", session.User.ID))
	if record.rotationDue(now) {
		if session.RotatedToken, err = rotateSession(ctx, tx, record.currentHash, now); err != nil {
			return Session{}, fmt.Errorf("rotate session: %w", err)
		}
	} else {
		session.RotatedToken = record.pendingToken(now)
		if err := touchSession(ctx, tx, record.currentHash, now); err != nil {
			return Session{}, fmt.Errorf("refresh session activity: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session check: %w", err)
	}
	return session, nil
}

func (s *Service) Logout(ctx context.Context, token string) (activityErr error) {
	ctx, activity := telemetry.StartActivity(ctx, "auth_logout")
	defer activity.End(&activityErr)

	if token == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM access_sessions WHERE token_hash = $1 OR previous_token_hash = $1`, tokenHash(token)); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func (s *Service) removeStaleSessions(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE access_sessions SET current_token_during_grace = NULL, current_token_grace_expires_at = NULL WHERE current_token_grace_expires_at <= $1`, now); err != nil {
		return fmt.Errorf("clean expired rotation tokens: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM access_sessions WHERE absolute_expires_at <= $1 OR last_seen_at <= $2`, now, now.Add(-idleTimeout)); err != nil {
		return fmt.Errorf("clean expired sessions: %w", err)
	}
	return nil
}

func loadSession(ctx context.Context, tx *sql.Tx, hash []byte, now time.Time) (sessionRecord, error) {
	var r sessionRecord
	err := tx.QueryRowContext(ctx, `
		SELECT s.token_hash, (s.token_hash = $1), s.current_token_during_grace, s.current_token_grace_expires_at, s.csrf_token,
		       s.last_seen_at, s.rotated_at, s.rotation_required, s.absolute_expires_at,
		       u.id::text, u.username, u.is_platform_admin, u.is_active, u.created_at
		FROM access_sessions s JOIN access_users u ON u.id = s.user_id
		WHERE s.token_hash = $1 OR (s.previous_token_hash = $1 AND s.previous_expires_at > $2)
		FOR UPDATE OF s`, hash, now).Scan(&r.currentHash, &r.presentedCurrent, &r.graceToken, &r.graceExpiry,
		&r.session.CSRFToken, &r.lastSeen, &r.rotatedAt, &r.rotationRequired, &r.session.AbsoluteExpiry,
		&r.session.User.ID, &r.session.User.Username, &r.session.User.PlatformAdmin, &r.session.User.Active,
		&r.session.User.CreatedAt)
	return r, err
}

func (r sessionRecord) expired(now time.Time) bool {
	return !r.session.User.Active || !now.Before(r.session.AbsoluteExpiry) || !now.Before(r.lastSeen.Add(idleTimeout))
}

// rotationDue rotates only on the current identifier. A request that still
// carries the previous identifier receives the replacement through pendingToken.
func (r sessionRecord) rotationDue(now time.Time) bool {
	return r.presentedCurrent && (r.rotationRequired || !now.Before(r.rotatedAt.Add(rotationInterval)))
}

func (r sessionRecord) pendingToken(now time.Time) string {
	if r.presentedCurrent || !r.graceToken.Valid || !r.graceExpiry.Valid || !now.Before(r.graceExpiry.Time) {
		return ""
	}
	return r.graceToken.String
}

func rotateSession(ctx context.Context, tx *sql.Tx, currentHash []byte, now time.Time) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE access_sessions SET previous_token_hash = token_hash, previous_expires_at = $1, token_hash = $2, current_token_during_grace = $3, current_token_grace_expires_at = $1, rotation_required = FALSE, last_seen_at = $4, rotated_at = $4 WHERE token_hash = $5`,
		now.Add(rotationGrace), tokenHash(token), token, now, currentHash)
	if err != nil {
		return "", err
	}
	return token, nil
}

func touchSession(ctx context.Context, tx *sql.Tx, hash []byte, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE access_sessions SET last_seen_at = $1, current_token_during_grace = CASE WHEN current_token_grace_expires_at <= $1 THEN NULL ELSE current_token_during_grace END, current_token_grace_expires_at = CASE WHEN current_token_grace_expires_at <= $1 THEN NULL ELSE current_token_grace_expires_at END WHERE token_hash = $2`, now, hash)
	return err
}

// requireSessionRotation defers rotation to the next authenticated request so
// the affected browser receives the new identifier in that response.
func requireSessionRotation(ctx context.Context, tx *sql.Tx, scope sessionScope, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE access_sessions SET rotation_required = TRUE WHERE `+string(scope), id)
	return err
}

func revokeSessions(ctx context.Context, tx *sql.Tx, scope sessionScope, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM access_sessions WHERE `+string(scope), id)
	return err
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
