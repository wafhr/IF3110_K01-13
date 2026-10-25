package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

type Status string

const (
	Running Status = "running"
	Success Status = "success"
	Failed  Status = "failed"
)

var ErrInvalidMigration = errors.New("invalid migration request")

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
type TargetOpener func(context.Context, string) (*sql.DB, database.DBMS, error)

type Service struct {
	openTarget TargetOpener
}

// NewService uses the database registry to open each target connection.
func NewService(openTarget TargetOpener) *Service { return &Service{openTarget: openTarget} }

func (s *Service) target(ctx context.Context, id string) (*sql.DB, database.DBMS, error) {
	return s.openTarget(ctx, id)
}

func (s *Service) List(ctx context.Context, databaseID string) ([]Record, error) {
	db, dbms, err := s.target(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db, dbms); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations ORDER BY version DESC`)
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
	db, dbms, err := s.target(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	if err := ensureHistory(ctx, db, dbms); err != nil {
		return Record{}, err
	}
	var row Record
	err = db.QueryRowContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations WHERE version=`+placeholder(dbms, 1), version).Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt)
	return row, err
}

func (s *Service) Execute(ctx context.Context, databaseID, userID, name, statement string) (Record, error) {
	name = strings.TrimSpace(name)
	statement = strings.TrimSpace(statement)
	if name == "" || len(name) > 160 || statement == "" || len(statement) > 1<<20 {
		return Record{}, fmt.Errorf("%w: migration name and SQL are required; limits are 160 and 1048576 characters", ErrInvalidMigration)
	}
	db, dbms, err := s.target(ctx, databaseID)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()
	if transactionControl(statement, dbms) {
		return Record{}, fmt.Errorf("%w: transaction control statements are managed by the migration runner", ErrInvalidMigration)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return Record{}, err
	}
	defer conn.Close()
	unlock, err := acquireLock(ctx, conn, databaseID, dbms)
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	if err := ensureHistory(ctx, conn, dbms); err != nil {
		return Record{}, err
	}
	row, err := insertRunning(ctx, conn, dbms, name, statement, userID)
	if err != nil {
		return Record{}, fmt.Errorf("record migration: %w", err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, dbms, row.Version)
		row.Status = Failed
		finished := time.Now().UTC()
		row.FinishedAt = &finished
		return row, fmt.Errorf("migration execution failed (version %d): %w", row.Version, err)
	}
	finished := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE platform_migrations SET status='success',finished_at=`+placeholder(dbms, 1)+` WHERE version=`+placeholder(dbms, 2), finished, row.Version); err != nil {
		_ = tx.Rollback()
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	if err = tx.Commit(); err != nil {
		s.markFailed(ctx, conn, dbms, row.Version)
		return row, err
	}
	row.Status = Success
	row.FinishedAt = &finished
	return row, nil
}

func transactionControl(statement string, dbms database.DBMS) bool {
	return transactionControlWithBackslashMode(statement, dbms, false) || transactionControlWithBackslashMode(statement, dbms, true)
}

func transactionControlWithBackslashMode(statement string, dbms database.DBMS, backslashEscapes bool) bool {
	if dbms == database.MySQL && mysqlExecutableControl(statement, backslashEscapes) {
		return true
	}
	return sqlStatementsContainTransactionControl(statement, dbms, backslashEscapes)
}

func sqlStatementsContainTransactionControl(statement string, dbms database.DBMS, backslashEscapes bool) bool {
	for position := 0; position < len(statement); {
		position = skipSQLTrivia(statement, position, dbms)
		if position >= len(statement) {
			return false
		}
		first, end := sqlWord(statement, position)
		next := skipSQLTrivia(statement, end, dbms)
		second, secondEnd := sqlWord(statement, next)
		if forbiddenTransactionStatement(first, second, statement, end, secondEnd, dbms, backslashEscapes) {
			return true
		}
		position = nextSQLStatement(statement, end, dbms, backslashEscapes)
	}
	return false
}

func forbiddenTransactionStatement(first, second, statement string, firstEnd, secondEnd int, dbms database.DBMS, backslashEscapes bool) bool {
	switch first {
	case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE", "XA":
		return true
	case "LOCK", "UNLOCK":
		return dbms == database.MySQL && second == "TABLES"
	case "SET":
		if second == "TRANSACTION" || strings.Contains(second, "AUTOCOMMIT") ||
			(dbms == database.MySQL && sqlContainsWord(statement, firstEnd, dbms, backslashEscapes, "AUTOCOMMIT")) {
			return true
		}
		if second == "LOCAL" || second == "SESSION" || second == "GLOBAL" {
			next := skipSQLTrivia(statement, secondEnd, dbms)
			third, _ := sqlWord(statement, next)
			return third == "TRANSACTION" || strings.Contains(third, "AUTOCOMMIT")
		}
	}
	return false
}

func sqlWord(statement string, position int) (string, int) {
	start := position
	for position < len(statement) {
		b := statement[position]
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') &&
			b != '_' && b != '$' && b != '@' && b != '.' {
			break
		}
		position++
	}
	return strings.ToUpper(statement[start:position]), position
}

func skipSQLTrivia(statement string, position int, dbms database.DBMS) int {
	for position < len(statement) {
		if strings.ContainsRune(" \t\r\n\f", rune(statement[position])) {
			position++
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "--" &&
			(dbms != database.MySQL || position+2 == len(statement) || statement[position+2] <= ' ') {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if dbms == database.MySQL && statement[position] == '#' {
			position = skipSQLLine(statement, position+1)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		break
	}
	return position
}

func skipSQLLine(statement string, position int) int {
	for position < len(statement) && statement[position] != '\n' && statement[position] != '\r' {
		position++
	}
	return position
}

func skipSQLBlockComment(statement string, position int) int {
	depth := 1
	position += 2
	for position < len(statement) && depth > 0 {
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			depth++
			position += 2
		} else if position+1 < len(statement) && statement[position:position+2] == "*/" {
			depth--
			position += 2
		} else {
			position++
		}
	}
	return position
}

func nextSQLStatement(statement string, position int, dbms database.DBMS, backslashEscapes bool) int {
	for position < len(statement) {
		if position+1 < len(statement) && statement[position:position+2] == "--" &&
			(dbms != database.MySQL || position+2 == len(statement) || statement[position+2] <= ' ') {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if dbms == database.MySQL && statement[position] == '#' {
			position = skipSQLLine(statement, position+1)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		if statement[position] == ';' {
			return position + 1
		}
		quote := statement[position]
		if quote == '\'' || quote == '"' || (quote == '`' && dbms == database.MySQL) {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes)
			continue
		}
		if dbms == database.PostgreSQL && statement[position] == '$' &&
			(position == 0 || !isSQLWordByte(statement[position-1])) {
			if end := dollarQuoteEnd(statement, position); end > position {
				position = end
				continue
			}
		}
		position++
	}
	return len(statement)
}

func skipSQLQuoted(statement string, position int, quote byte, backslashEscapes bool) int {
	position++
	for position < len(statement) {
		if statement[position] == quote {
			if position+1 < len(statement) && statement[position+1] == quote {
				position += 2
				continue
			}
			return position + 1
		}
		if statement[position] == '\\' && backslashEscapes && position+1 < len(statement) {
			position += 2
			continue
		}
		position++
	}
	return position
}

func sqlContainsWord(statement string, position int, dbms database.DBMS, backslashEscapes bool, target string) bool {
	for position < len(statement) {
		position = skipSQLTrivia(statement, position, dbms)
		if position >= len(statement) || statement[position] == ';' {
			return false
		}
		quote := statement[position]
		if quote == '\'' || quote == '"' || (quote == '`' && dbms == database.MySQL) {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes)
			continue
		}
		if dbms == database.PostgreSQL && quote == '$' &&
			(position == 0 || !isSQLWordByte(statement[position-1])) {
			if end := dollarQuoteEnd(statement, position); end > position {
				position = end
				continue
			}
		}
		word, end := sqlWord(statement, position)
		if strings.Contains(word, target) {
			return true
		}
		if end == position {
			position++
		} else {
			position = end
		}
	}
	return false
}

func isSQLWordByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

func dollarQuoteEnd(statement string, position int) int {
	end := position + 1
	for end < len(statement) && isSQLWordByte(statement[end]) {
		end++
	}
	if end >= len(statement) || statement[end] != '$' || (end > position+1 && statement[position+1] >= '0' && statement[position+1] <= '9') {
		return position
	}
	delimiter := statement[position : end+1]
	closing := strings.Index(statement[end+1:], delimiter)
	if closing < 0 {
		return len(statement)
	}
	return end + 1 + closing + len(delimiter)
}

func mysqlExecutableControl(statement string, backslashEscapes bool) bool {
	for position := 0; ; {
		start := strings.Index(statement[position:], "/*!")
		if start < 0 {
			return false
		}
		start += position + 3
		end := strings.Index(statement[start:], "*/")
		if end < 0 {
			return false
		}
		end += start
		for start < end && (statement[start] >= '0' && statement[start] <= '9' || statement[start] <= ' ') {
			start++
		}
		commentBody := statement[start:end]
		if strings.Contains(commentBody, "/*!") || strings.Contains(strings.ToUpper(commentBody), "AUTOCOMMIT") ||
			sqlStatementsContainTransactionControl(commentBody, database.MySQL, backslashEscapes) {
			return true
		}
		position = end + 2
	}
}

func ensureHistory(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, dbms database.DBMS) error {
	statement := `CREATE TABLE IF NOT EXISTS platform_migrations (version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,name TEXT NOT NULL,sql TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('running','success','failed')),executed_by UUID NOT NULL,started_at TIMESTAMPTZ NOT NULL DEFAULT now(),finished_at TIMESTAMPTZ)`
	if dbms == database.MySQL {
		statement = "CREATE TABLE IF NOT EXISTS platform_migrations (version BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,name VARCHAR(160) NOT NULL,`sql` LONGTEXT NOT NULL,status VARCHAR(16) NOT NULL CHECK(status IN ('running','success','failed')),executed_by CHAR(36) NOT NULL,started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),finished_at DATETIME(6) NULL) ENGINE=InnoDB"
	}
	_, err := db.ExecContext(ctx, statement)
	return err
}

func (s *Service) markFailed(ctx context.Context, conn *sql.Conn, dbms database.DBMS, version int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `UPDATE platform_migrations SET status='failed',finished_at=CURRENT_TIMESTAMP WHERE version=`+placeholder(dbms, 1)+` AND status='running'`, version)
}

func historyColumns(dbms database.DBMS) string {
	if dbms == database.MySQL {
		return "version,name,`sql`,status,executed_by,started_at,finished_at"
	}
	return "version,name,sql,status,executed_by::text,started_at,finished_at"
}

func placeholder(dbms database.DBMS, position int) string {
	if dbms == database.MySQL {
		return "?"
	}
	return fmt.Sprintf("$%d", position)
}

func acquireLock(ctx context.Context, conn *sql.Conn, id string, dbms database.DBMS) (func(), error) {
	if dbms == database.MySQL {
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 5)`, "if3110_migration_"+id).Scan(&acquired); err != nil {
			return nil, err
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return nil, errors.New("could not acquire migration lock")
		}
		return func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(?)`, "if3110_migration_"+id)
		}, nil
	}
	if dbms != database.PostgreSQL {
		return nil, database.ErrInvalidInput
	}
	lock := int64(databaseLock(id))
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lock); err != nil {
		return nil, err
	}
	return func() { _, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lock) }, nil
}

func insertRunning(ctx context.Context, conn *sql.Conn, dbms database.DBMS, name, statement, userID string) (Record, error) {
	var row Record
	if dbms == database.PostgreSQL {
		err := conn.QueryRowContext(ctx, `INSERT INTO platform_migrations(name,sql,status,executed_by) VALUES($1,$2,'running',$3) RETURNING version,name,sql,status,executed_by::text,started_at`, name, statement, userID).
			Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt)
		return row, err
	}
	if dbms != database.MySQL {
		return row, database.ErrInvalidInput
	}
	result, err := conn.ExecContext(ctx, "INSERT INTO platform_migrations(name,`sql`,status,executed_by) VALUES(?,?,'running',?)", name, statement, userID)
	if err != nil {
		return row, err
	}
	row.Version, err = result.LastInsertId()
	if err != nil {
		return row, err
	}
	err = conn.QueryRowContext(ctx, `SELECT `+historyColumns(dbms)+` FROM platform_migrations WHERE version=?`, row.Version).
		Scan(&row.Version, &row.Name, &row.SQL, &row.Status, &row.ExecutedBy, &row.StartedAt, &row.FinishedAt)
	return row, err
}
func databaseLock(id string) uint32 { h := fnv.New32a(); _, _ = h.Write([]byte(id)); return h.Sum32() }
