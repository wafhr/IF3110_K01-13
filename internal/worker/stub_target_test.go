package worker

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/testsql"
)

type stubCall struct {
	query string
	args  []driver.Value
}

type stubQuery struct {
	fragment  string
	rows      [][]driver.Value
	remaining int
}

// stubTarget is a target database that records statements, arguments, and
// transaction outcomes, answers queries by statement fragment, and injects
// failures.
type stubTarget struct {
	mu         sync.Mutex
	calls      []stubCall
	queries    []stubQuery
	isolation  driver.IsolationLevel
	execErrors map[string]error
	commitErr  error
	commits    int
	rollbacks  int
}

func newStubTarget() *stubTarget { return &stubTarget{execErrors: map[string]error{}} }

func (s *stubTarget) onQuery(fragment string, rows ...[]driver.Value) *stubTarget {
	s.queries = append(s.queries, stubQuery{fragment: fragment, rows: rows})
	return s
}

// onQueryOnce answers the first matching query only; later ones fall through
// to the next matching rule.
func (s *stubTarget) onQueryOnce(fragment string, rows ...[]driver.Value) *stubTarget {
	s.queries = append(s.queries, stubQuery{fragment: fragment, rows: rows, remaining: 1})
	return s
}

func (s *stubTarget) open(t *testing.T) *sql.DB { return testsql.Open(t, s) }

func (s *stubTarget) ran(fragment string) bool { return len(s.executed(fragment)) > 0 }

func (s *stubTarget) executed(fragment string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []stubCall
	for _, call := range s.calls {
		if strings.Contains(call.query, fragment) {
			matches = append(matches, call)
		}
	}
	return matches
}

func (s *stubTarget) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	statements := make([]string, len(s.calls))
	for i, call := range s.calls {
		statements[i] = call.query
	}
	return statements
}

func (s *stubTarget) record(query string, args []driver.NamedValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{query: query, args: testsql.Values(args)})
}

func (s *stubTarget) Exec(query string, args []driver.NamedValue) (driver.Result, error) {
	s.record(query, args)
	s.mu.Lock()
	defer s.mu.Unlock()
	for fragment, err := range s.execErrors {
		if strings.Contains(query, fragment) {
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

func (s *stubTarget) Query(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.record(query, args)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rule := range s.queries {
		if rule.remaining < 0 || !strings.Contains(query, rule.fragment) {
			continue
		}
		if rule.remaining == 1 {
			s.queries[i].remaining = -1
		}
		if len(rule.rows) == 0 {
			return &testsql.Rows{Names: []string{"value"}}, nil
		}
		return &testsql.Rows{Values: rule.rows}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (s *stubTarget) Begin(options driver.TxOptions) error {
	s.mu.Lock()
	s.isolation = options.Isolation
	s.mu.Unlock()
	s.record("BEGIN", nil)
	return nil
}

func (s *stubTarget) Commit() error   { return s.finish("COMMIT", true) }
func (s *stubTarget) Rollback() error { return s.finish("ROLLBACK", false) }

func (s *stubTarget) finish(statement string, commit bool) error {
	s.record(statement, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !commit {
		s.rollbacks++
		return nil
	}
	if s.commitErr != nil {
		return s.commitErr
	}
	s.commits++
	return nil
}

