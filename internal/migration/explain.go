package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

var errInvalidPlanQuery = errors.New("invalid or unsupported SQL for query plan preview")

// QueryPlan retains the target database's native plan output and preview mode.
type QueryPlan struct {
	Mode       string          `json:"mode"`
	Plan       json.RawMessage `json:"plan"`
	RolledBack bool            `json:"rolled_back"`
}

func (s *Service) Explain(ctx context.Context, databaseID, statement string, analyze bool) (QueryPlan, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" || len(statement) > 1<<20 || strings.ContainsRune(statement, '\x00') {
		return QueryPlan{}, errInvalidPlanQuery
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, dbms, err := s.target(ctx, databaseID)
	if err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	defer db.Close()
	if dbms != database.PostgreSQL && dbms != database.MySQL {
		return QueryPlan{}, errInvalidPlanQuery
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !analyze})
	if err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	defer tx.Rollback()
	if dbms == database.PostgreSQL {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '5s'; SET LOCAL lock_timeout = '1s'`); err != nil {
			return QueryPlan{}, planError(ctx, err, dbms)
		}
	}
	mode, prefix := "explain", "EXPLAIN (FORMAT JSON, ANALYZE FALSE) "
	if dbms == database.MySQL {
		prefix = "EXPLAIN FORMAT=JSON "
	}
	if analyze {
		mode = "explain_analyze"
		if dbms == database.MySQL {
			prefix = "EXPLAIN ANALYZE FORMAT=TREE "
		} else {
			prefix = "EXPLAIN (FORMAT JSON, ANALYZE TRUE, BUFFERS TRUE) "
		}
	}
	// Prepare makes the database reject multiple statements without relying on
	// a SQL splitter that could mishandle dialect-specific quoting and comments.
	prepared, err := tx.PrepareContext(ctx, prefix+statement)
	if err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	defer prepared.Close()
	var plan []byte
	if dbms == database.MySQL && analyze {
		var tree string
		if err := prepared.QueryRowContext(ctx).Scan(&tree); err != nil {
			return QueryPlan{}, planError(ctx, err, dbms)
		}
		plan, err = json.Marshal(tree)
	} else if err := prepared.QueryRowContext(ctx).Scan(&plan); err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	if err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	if !json.Valid(plan) {
		return QueryPlan{}, errors.New("invalid query plan response")
	}
	// Never commit a preview. Confirm rollback before reporting success; sequence
	// increments and external function/trigger effects are not transactional.
	if err := tx.Rollback(); err != nil {
		return QueryPlan{}, planError(ctx, err, dbms)
	}
	return QueryPlan{Mode: mode, Plan: plan, RolledBack: true}, nil
}

// Database errors can include user SQL and server details. Return only safe
// categories; the HTTP handler supplies a useful message without those details.
func planError(ctx context.Context, err error, dbms database.DBMS) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if dbms == database.MySQL {
		var mysqlErr *drivermysql.MySQLError
		if errors.As(err, &mysqlErr) {
			switch mysqlErr.Number {
			case 1044, 1142, 1143, 1227:
				return targetdb.ErrPermissionDenied
			case 1045:
				return targetdb.ErrAuthentication
			case 1205, 3024:
				return context.DeadlineExceeded
			}
			state := string(mysqlErr.SQLState[:])
			if strings.HasPrefix(state, "42") || strings.HasPrefix(state, "22") || state == "0A000" {
				return errInvalidPlanQuery
			}
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		switch code {
		case "42501":
			return targetdb.ErrPermissionDenied
		case "57014", "55P03":
			return context.DeadlineExceeded
		}
		if strings.HasPrefix(code, "42") || strings.HasPrefix(code, "22") || code == "0A000" {
			return errInvalidPlanQuery
		}
	}
	return targetdb.ConnectionError(err)
}
