package job

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/testsql"
)

type statement struct {
	query string
	args  []driver.Value
}

type queryRule struct {
	fragment string
	rows     [][]driver.Value
	err      error
}

type execRule struct {
	fragment string
	affected int64
	err      error
}

// stubDatabase answers SQL by statement fragment and records what was executed.
type stubDatabase struct {
	mu         sync.Mutex
	queries    []queryRule
	execs      []execRule
	statements []statement
}

func (s *stubDatabase) onQuery(fragment string, rows ...[]driver.Value) *stubDatabase {
	s.queries = append(s.queries, queryRule{fragment: fragment, rows: rows})
	return s
}

func (s *stubDatabase) failQuery(fragment string, err error) *stubDatabase {
	s.queries = append(s.queries, queryRule{fragment: fragment, err: err})
	return s
}

func (s *stubDatabase) onExec(fragment string, affected int64) *stubDatabase {
	s.execs = append(s.execs, execRule{fragment: fragment, affected: affected})
	return s
}

func (s *stubDatabase) executed(fragment string) []statement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []statement
	for _, st := range s.statements {
		if strings.Contains(st.query, fragment) {
			matches = append(matches, st)
		}
	}
	return matches
}

func (s *stubDatabase) repository(t *testing.T) *Repository {
	t.Helper()
	repository, err := NewRepository(testsql.Open(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func (s *stubDatabase) record(query string, args []driver.NamedValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, statement{query: query, args: testsql.Values(args)})
}

func (s *stubDatabase) Query(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.record(query, args)
	for _, rule := range s.queries {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			columns := jobColumnNames
			if rule.fragment == jobClaim {
				columns = append(append([]string(nil), columns...), "wait_seconds")
			}
			return &testsql.Rows{Names: columns, Values: rule.rows}, nil
		}
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (s *stubDatabase) Exec(query string, args []driver.NamedValue) (driver.Result, error) {
	s.record(query, args)
	for _, rule := range s.execs {
		if strings.Contains(query, rule.fragment) {
			if rule.err != nil {
				return nil, rule.err
			}
			return driver.RowsAffected(rule.affected), nil
		}
	}
	return driver.RowsAffected(1), nil
}

var jobColumnNames = []string{
	"id", "payload", "status", "operation", "attempts", "committed_batches", "committed_rows",
	"total_rows", "last_error", "available_at", "created_at", "started_at", "finished_at",
}
