package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

type Status string

const (
	Running Status = "running"
	Success Status = "success"
	Failed  Status = "failed"
)

type Record struct {
	Version    int64      `json:"version"`
	Name       string     `json:"name"`
	SQL        string     `json:"sql"`
	Status     Status     `json:"status"`
	ExecutedBy string     `json:"executed_by"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// TargetOpener returns a new pool owned by the migration operation. It must not
// return a shared viewer pool because the runner closes it after each operation.
type TargetOpener func(context.Context, string) (*sql.DB, error)

type Service struct {
	openTarget TargetOpener
}

// NewService uses the database registry to open each target connection.
func NewService(openTarget TargetOpener) *Service { return &Service{openTarget: openTarget} }

func (s *Service) target(ctx context.Context, id string) (*sql.DB, error) {
	return s.openTarget(ctx, id)
}

func (s *Service) List(ctx context.Context, databaseID string) ([]Record, error) {
	db, err := s.target(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT version, name, sql, status, executed_by::text, started_at, finished_at FROM platform_migrations ORDER BY version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Record, 0)
	for rows.Next() {
		var row Record
		if err := rows.Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Service) Get(ctx context.Context, databaseID string, version int64) (Record, error) {
	db, err := s.target(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db); err != nil {
		return Record{}, err
	}
	var row Record
	err = db.QueryRowContext(ctx, `SELECT version,name,sql,status,executed_by::text,started_at,finished_at FROM platform_migrations WHERE version=$1`, version).Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt)
	return row, err
}

func (s *Service) Execute(ctx context.Context, databaseID, userID, name, statement string) (Record, error) {
	name = strings.TrimSpace(name)
	statement = strings.TrimSpace(statement)
	if name == "" || len(name) > 160 || statement == "" || len(statement) > 1<<20 {
		return Record{}, errors.New("migration name and SQL are required; limits are 160 and 1048576 characters")
	}
	if transactionControl(statement) {
		return Record{}, errors.New("transaction control statements are managed by the migration runner")
	}
	db, err := s.target(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return Record{}, err
	}
	defer conn.Close()
	lock := int64(databaseLock(databaseID))
	if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lock); err != nil {
		return Record{}, err
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lock)
	if err := ensureHistory(ctx, conn); err != nil {
		return Record{}, err
	}
	var row Record
	err = conn.QueryRowContext(ctx, `INSERT INTO platform_migrations(name,sql,status,executed_by) VALUES($1,$2,'running',$3) RETURNING version,name,sql,status,executed_by::text,started_at`, name, statement, userID).Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt)
	if err != nil {
		return Record{}, fmt.Errorf("record migration: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		s.markFailed(ctx, conn, row.Version)
		return row, err
	}
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, row.Version)
		row.Status = Failed
		finished := time.Now().UTC()
		row.FinishedAt = &finished
		return row, fmt.Errorf("migration execution failed (version %d): %w", row.Version, err)
	}
	finished := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE platform_migrations SET status='success',finished_at=$1 WHERE version=$2`, finished, row.Version); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, row.Version)
		return row, err
	}
	if err = tx.Commit(); err != nil {
		s.markFailed(ctx, conn, row.Version)
		return row, err
	}
	row.Status = Success
	row.FinishedAt = &finished
	return row, nil
}

func transactionControl(statement string) bool {
	text := strings.TrimSpace(statement)
	for {
		if strings.HasPrefix(text, "--") {
			if end := strings.IndexByte(text, '\n'); end >= 0 {
				text = strings.TrimSpace(text[end+1:])
				continue
			}
			return false
		}
		if strings.HasPrefix(text, "/*") {
			end := strings.Index(text[2:], "*/")
			if end < 0 {
				return false
			}
			text = strings.TrimSpace(text[end+4:])
			continue
		}
		break
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToUpper(strings.Trim(fields[0], ";"))
	switch first {
	case "BEGIN", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE":
		return true
	case "SET":
		return len(fields) > 1 && strings.EqualFold(strings.Trim(fields[1], ";"), "TRANSACTION")
	default:
		return false
	}
}

func ensureHistory(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS platform_migrations (version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,name TEXT NOT NULL,sql TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('running','success','failed')),executed_by UUID NOT NULL,started_at TIMESTAMPTZ NOT NULL DEFAULT now(),finished_at TIMESTAMPTZ)`)
	return err
}

func (s *Service) markFailed(ctx context.Context, conn *sql.Conn, version int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `UPDATE platform_migrations SET status='failed',finished_at=now() WHERE version=$1 AND status='running'`, version)
}
func databaseLock(id string) uint32 { h := fnv.New32a(); _, _ = h.Write([]byte(id)); return h.Sum32() }
