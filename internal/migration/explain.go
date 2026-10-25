package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/targetdb"
)

var errInvalidPlanQuery = errors.New("invalid or unsupported SQL for query plan preview")

// QueryPlan retains PostgreSQL's native plan tree and identifies the preview mode.
type QueryPlan struct {
	Mode string          `json:"mode"`
	Plan json.RawMessage `json:"plan"`
}

func (s *Service) Explain(ctx context.Context, databaseID, statement string) (QueryPlan, error) {
	statement = strings.TrimSpace(statement)
	if statement == "" || len(statement) > 1<<20 || strings.ContainsRune(statement, '\x00') {
		return QueryPlan{}, errInvalidPlanQuery
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := s.target(ctx, databaseID)
	if err != nil {
		return QueryPlan{}, planError(ctx, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return QueryPlan{}, planError(ctx, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '5s'; SET LOCAL lock_timeout = '1s'`); err != nil {
		return QueryPlan{}, planError(ctx, err)
	}
	// Preparing requires exactly one statement, including when SQL contains
	// comments, quoted semicolons, or dollar-quoted strings. Do not split SQL by
	// semicolons or send user SQL through the simple-query protocol.
	prepared, err := tx.PrepareContext(ctx, "EXPLAIN (FORMAT JSON, ANALYZE FALSE) "+statement)
	if err != nil {
		return QueryPlan{}, planError(ctx, err)
	}
	defer prepared.Close()
	var plan []byte
	if err := prepared.QueryRowContext(ctx).Scan(&plan); err != nil {
		return QueryPlan{}, planError(ctx, err)
	}
	if !json.Valid(plan) {
		return QueryPlan{}, errors.New("invalid query plan response")
	}
	return QueryPlan{Mode: "explain", Plan: plan}, nil
}

// PostgreSQL errors can include user SQL and server details. Return only safe
// categories; the HTTP handler supplies a useful message without those details.
func planError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
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
