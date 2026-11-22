package job

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFinishingTransitionsRecordTheAttempt(t *testing.T) {
	tests := map[string]struct {
		run    func(*Repository) error
		status string
		reason string
	}{
		"complete": {run: func(r *Repository) error { return r.Complete(context.Background(), testJobID, testWorkerID) },
			status: "updated.status", reason: "''"},
		"fail": {run: func(r *Repository) error {
			return r.Fail(context.Background(), testJobID, testWorkerID, "syntax error", false)
		}, status: "updated.status", reason: "updated.last_error"},
		"retry": {run: func(r *Repository) error {
			return r.Retry(context.Background(), testJobID, testWorkerID, "connection reset", time.Second)
		}, status: "'retrying'", reason: "updated.last_error"},
		"release": {run: func(r *Repository) error { return r.Release(context.Background(), testJobID, testWorkerID) },
			status: "'released'", reason: "'worker stopped before the attempt finished'"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			database := &stubDatabase{}
			if err := test.run(database.repository(t)); err != nil {
				t.Fatal(err)
			}
			statements := database.executed("INSERT INTO migration_attempts")
			if len(statements) != 1 {
				t.Fatalf("attempt inserts = %d, want 1", len(statements))
			}
			query := statements[0].query
			for _, fragment := range []string{"WITH updated AS (UPDATE migration_jobs j", leaseHolder, "FROM updated",
				test.status + ", " + test.reason, "payload #>> '{plan,sql}'", "payload #>> '{plan,compensation_sql}'"} {
				if !strings.Contains(query, fragment) {
					t.Errorf("attempt statement misses %q", fragment)
				}
			}
		})
	}
}

func TestProgressAndHeartbeatDoNotEndTheAttempt(t *testing.T) {
	database := &stubDatabase{}
	repository := database.repository(t)
	if err := repository.Heartbeat(context.Background(), testJobID, testWorkerID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := repository.UpdateProgress(context.Background(), testJobID, testWorkerID, Progress{CommittedBatches: 1}); err != nil {
		t.Fatal(err)
	}
	if inserts := database.executed("INSERT INTO migration_attempts"); len(inserts) != 0 {
		t.Fatalf("heartbeat or progress recorded %d attempts", len(inserts))
	}
}

func TestClaimRecordsTheInterruptedAttempt(t *testing.T) {
	database := (&stubDatabase{}).onQuery(jobClaim, append(jobRow(t, testRequest(), Running, 2), float64(0)))
	if _, err := database.repository(t).Claim(context.Background(), testWorkerID, time.Minute); err != nil {
		t.Fatal(err)
	}
	claim := database.executed(jobClaim)[0].query
	for _, fragment := range []string{"interrupted AS (", "INSERT INTO migration_attempts", "'interrupted'", "WHERE old.status = 'running'",
		"current_attempt = j.attempts + 1", "attempt_started_at = now()"} {
		if !strings.Contains(claim, fragment) {
			t.Errorf("claim misses %q", fragment)
		}
	}
}

func TestListAttemptsValidatesInput(t *testing.T) {
	repository := (&stubDatabase{}).repository(t)
	if _, err := repository.ListAttempts(context.Background(), "database", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed database ID returned %v", err)
	}
	for _, limit := range []int{0, MaxListLimit + 1} {
		if _, err := repository.ListAttempts(context.Background(), testDatabaseID, limit); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("limit %d returned %v", limit, err)
		}
	}
}

func TestListAttemptsAPI(t *testing.T) {
	store := newFakeStore()
	store.attempts = []Attempt{{ID: "attempt-1", JobID: testJobID, Name: "add index", Attempt: 1, Status: "failed",
		SQL: "CREATE INDEX orders_customer_idx ON orders (customer_id)", Error: "SQLSTATE 42P01: relation does not exist"}}
	response := serve(t, store, allowGuard(t), "/databases/"+testDatabaseID+"/attempts?limit=20")
	if response.Code != http.StatusOK || store.listedFor != testDatabaseID || store.listedLimit != 20 {
		t.Fatalf("status = %d, listed %q/%d", response.Code, store.listedFor, store.listedLimit)
	}
	var body struct {
		Attempts []Attempt `json:"attempts"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Attempts) != 1 || body.Attempts[0].SQL == "" || body.Attempts[0].Status != "failed" {
		t.Fatalf("attempts = %+v", body.Attempts)
	}
	if response := serve(t, store, allowGuard(t), "/databases/"+testDatabaseID+"/attempts?limit=0"); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d", response.Code)
	}
	if response := serve(t, store, denyGuard(), "/databases/"+testDatabaseID+"/attempts"); response.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d", response.Code)
	}
}
