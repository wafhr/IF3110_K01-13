package job

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

const (
	jobInsert      = "INSERT INTO migration_jobs"
	jobClaim       = "FOR UPDATE SKIP LOCKED"
	jobByExecution = "WHERE j.execution_id = $1"
	jobByID        = "WHERE j.id = $1"
)

var testCreatedAt = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func jobRow(t *testing.T, request migration.ExecutionRequest, status Status, attempts int64) []driver.Value {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var startedAt driver.Value
	if attempts > 0 {
		startedAt = testCreatedAt.Add(time.Second)
	}
	return []driver.Value{testJobID, payload, string(status), string(Execute), attempts, int64(0), int64(0), int64(0), "", testCreatedAt, testCreatedAt, startedAt, nil}
}

func TestNewRepositoryRequiresDatabase(t *testing.T) {
	if _, err := NewRepository(nil); err == nil {
		t.Fatal("repository accepted a nil database")
	}
}

func TestEnqueueRejectsInvalidRequest(t *testing.T) {
	database := &stubDatabase{}
	request := testRequest()
	request.Plan.SQL = ""
	if _, err := database.repository(t).Enqueue(context.Background(), request); !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("error = %v, want ErrInvalidJob", err)
	}
	if len(database.executed("")) != 0 {
		t.Fatal("invalid request reached the database")
	}
}

func TestEnqueueStoresCompleteRequest(t *testing.T) {
	request := testRequest()
	database := (&stubDatabase{}).onQuery(jobInsert, jobRow(t, request, Pending, 0))
	job, err := database.repository(t).Enqueue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != testJobID || job.Status != Pending || job.ExecutionID != testExecutionID || job.StartedAt != nil || job.Request != request {
		t.Fatalf("enqueued job = %+v", job)
	}
	inserts := database.executed(jobInsert)
	if len(inserts) != 1 || inserts[0].args[0] != testExecutionID || inserts[0].args[1] != testDatabaseID {
		t.Fatalf("insert statements = %+v", inserts)
	}
	var stored migration.ExecutionRequest
	if err := json.Unmarshal(inserts[0].args[2].([]byte), &stored); err != nil || stored != request {
		t.Fatalf("stored payload = %+v, %v", stored, err)
	}
}

func TestEnqueueRedelivery(t *testing.T) {
	stored := testRequest()
	tests := []struct {
		name    string
		change  func(*migration.ExecutionRequest)
		wantErr error
	}{
		{name: "same execution returns the stored job", change: func(r *migration.ExecutionRequest) { r.TraceContext.TraceParent = "" }},
		{name: "different snapshot with the same execution is rejected", change: func(r *migration.ExecutionRequest) { r.Plan.Revision = 3 }, wantErr: ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := (&stubDatabase{}).onQuery(jobInsert).onQuery(jobByExecution, jobRow(t, stored, Running, 1))
			submitted := testRequest()
			test.change(&submitted)
			job, err := database.repository(t).Enqueue(context.Background(), submitted)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr == nil && (job.Status != Running || job.Request != stored) {
				t.Fatalf("redelivery returned %+v", job)
			}
		})
	}
}

func TestClaim(t *testing.T) {
	t.Run("rejects missing worker or lease", func(t *testing.T) {
		repository := (&stubDatabase{}).repository(t)
		if _, err := repository.Claim(context.Background(), "", time.Minute); err == nil {
			t.Fatal("claim accepted an empty worker ID")
		}
		if _, err := repository.Claim(context.Background(), testWorkerID, 0); err == nil {
			t.Fatal("claim accepted a zero lease")
		}
	})
	t.Run("reports an empty queue", func(t *testing.T) {
		database := (&stubDatabase{}).onQuery(jobClaim)
		if _, err := database.repository(t).Claim(context.Background(), testWorkerID, time.Minute); !errors.Is(err, ErrNoJob) {
			t.Fatalf("error = %v, want ErrNoJob", err)
		}
	})
	t.Run("leases the next job", func(t *testing.T) {
		database := (&stubDatabase{}).onQuery(jobClaim, jobRow(t, testRequest(), Running, 1))
		job, err := database.repository(t).Claim(context.Background(), testWorkerID, 90*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != Running || job.Attempts != 1 || job.StartedAt == nil || job.Request.TraceContext != testRequest().TraceContext {
			t.Fatalf("claimed job = %+v", job)
		}
		claim := database.executed(jobClaim)[0]
		if claim.args[0] != testWorkerID || claim.args[1] != int64(90000) {
			t.Fatalf("claim arguments = %v", claim.args)
		}
	})
}

func TestLeasedTransitions(t *testing.T) {
	transitions := map[string]func(*Repository, string) error{
		"heartbeat": func(r *Repository, id string) error {
			return r.Heartbeat(context.Background(), id, testWorkerID, time.Minute)
		},
		"complete": func(r *Repository, id string) error { return r.Complete(context.Background(), id, testWorkerID) },
		"fail": func(r *Repository, id string) error {
			return r.Fail(context.Background(), id, testWorkerID, "syntax error", false)
		},
		"release": func(r *Repository, id string) error {
			return r.Release(context.Background(), id, testWorkerID)
		},
		"retry": func(r *Repository, id string) error {
			return r.Retry(context.Background(), id, testWorkerID, "connection reset", 2*time.Second)
		},
		"progress": func(r *Repository, id string) error {
			return r.UpdateProgress(context.Background(), id, testWorkerID, Progress{CommittedBatches: 2, CommittedRows: 2000, TotalRows: 5000})
		},
	}
	for name, transition := range transitions {
		t.Run(name, func(t *testing.T) {
			owned := &stubDatabase{}
			if err := transition(owned.repository(t), testJobID); err != nil {
				t.Fatalf("owned transition failed: %v", err)
			}
			update := owned.executed("UPDATE migration_jobs")
			if len(update) != 1 || update[0].args[0] != testJobID || update[0].args[1] != testWorkerID {
				t.Fatalf("update = %+v", update)
			}
			lost := (&stubDatabase{}).onExec("UPDATE migration_jobs", 0)
			if err := transition(lost.repository(t), testJobID); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("transition without the lease returned %v", err)
			}
			if err := transition((&stubDatabase{}).repository(t), "job-1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("transition of a malformed ID returned %v", err)
			}
		})
	}
}

func TestTransitionArguments(t *testing.T) {
	database := &stubDatabase{}
	repository := database.repository(t)
	if err := repository.Retry(context.Background(), testJobID, testWorkerID, "connection reset", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fail(context.Background(), testJobID, testWorkerID, "syntax error", false); err != nil {
		t.Fatal(err)
	}
	updates := database.executed("UPDATE migration_jobs")
	if updates[0].args[2] != "connection reset" || updates[0].args[3] != int64(1500) {
		t.Fatalf("retry arguments = %v", updates[0].args)
	}
	if updates[1].args[2] != "syntax error" {
		t.Fatalf("fail arguments = %v", updates[1].args)
	}
	if err := repository.Retry(context.Background(), testJobID, testWorkerID, "x", -time.Second); err == nil {
		t.Fatal("retry accepted a negative delay")
	}
	if err := repository.Heartbeat(context.Background(), testJobID, testWorkerID, 0); err == nil {
		t.Fatal("heartbeat accepted a zero lease")
	}
}

func TestGet(t *testing.T) {
	missing := (&stubDatabase{}).onQuery(jobByID)
	if _, err := missing.repository(t).Get(context.Background(), testJobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job returned %v", err)
	}
	if _, err := (&stubDatabase{}).repository(t).Get(context.Background(), "job-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed ID returned %v", err)
	}
	corrupt := jobRow(t, testRequest(), Pending, 0)
	corrupt[1] = []byte("{")
	if _, err := (&stubDatabase{}).onQuery(jobByID, corrupt).repository(t).Get(context.Background(), testJobID); err == nil {
		t.Fatal("corrupt payload was accepted")
	}
	failing := (&stubDatabase{}).failQuery(jobByID, errors.New("connection refused"))
	if _, err := failing.repository(t).Get(context.Background(), testJobID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("storage failure returned %v", err)
	}
}

func TestListByDatabase(t *testing.T) {
	database := (&stubDatabase{}).onQuery("WHERE j.database_id = $1", jobRow(t, testRequest(), Pending, 0))
	jobs, err := database.repository(t).ListByDatabase(context.Background(), testDatabaseID, 20)
	if err != nil || len(jobs) != 1 || jobs[0].DatabaseID != testDatabaseID {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	list := database.executed("ORDER BY j.queue_position DESC")[0]
	if list.args[0] != testDatabaseID || list.args[1] != int64(20) {
		t.Fatalf("list arguments = %v", list.args)
	}
	if !strings.Contains(list.query, "j.payload #- '{plan,sql}' #- '{plan,compensation_sql}'") {
		t.Fatalf("listing loads the queued SQL: %s", list.query)
	}
	repository := (&stubDatabase{}).repository(t)
	if _, err := repository.ListByDatabase(context.Background(), "db", 20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed database ID returned %v", err)
	}
	for _, limit := range []int{0, MaxListLimit + 1} {
		if _, err := repository.ListByDatabase(context.Background(), testDatabaseID, limit); !errors.Is(err, ErrInvalidJob) {
			t.Fatalf("limit %d returned %v", limit, err)
		}
	}
}

func TestProgressAndStopStatements(t *testing.T) {
	database := &stubDatabase{}
	repository := database.repository(t)
	if err := repository.UpdateProgress(context.Background(), testJobID, testWorkerID, Progress{CommittedBatches: 2, CommittedRows: 2000, TotalRows: 5000}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fail(context.Background(), testJobID, testWorkerID, "division by zero", true); err != nil {
		t.Fatal(err)
	}
	if err := repository.Complete(context.Background(), testJobID, testWorkerID); err != nil {
		t.Fatal(err)
	}
	updates := database.executed("UPDATE migration_jobs")
	if args := updates[0].args; args[2] != int64(2) || args[3] != int64(2000) || args[4] != int64(5000) || !strings.Contains(updates[0].query, "touched = TRUE") {
		t.Fatalf("progress update = %+v", updates[0])
	}
	for _, fragment := range []string{"touched = touched OR $4", "WHEN NOT (touched OR $4) THEN 'failed'", "THEN 'partially_reverted'", "ELSE 'partial'", "finished_at = CASE WHEN touched OR $4 THEN NULL"} {
		if !strings.Contains(updates[1].query, fragment) {
			t.Errorf("fail statement misses %q", fragment)
		}
	}
	if updates[1].args[2] != "division by zero" || updates[1].args[3] != true {
		t.Fatalf("fail arguments = %v", updates[1].args)
	}
	if !strings.Contains(updates[2].query, "WHEN operation = 'compensate' THEN 'reverted' ELSE 'succeeded'") {
		t.Fatalf("complete statement = %s", updates[2].query)
	}
}

func TestRequeueStoppedJobs(t *testing.T) {
	tests := []struct {
		name      string
		requeue   func(*Repository, string) error
		condition string
		rejected  error
	}{
		{name: "resume", requeue: (*Repository).resumeFor, condition: "status IN ('partial', 'partially_reverted')", rejected: ErrNotResumable},
		{name: "cancel", requeue: (*Repository).cancelFor, condition: "operation = 'execute' AND payload #>> '{plan,action}' <> 'rollback'", rejected: ErrNotCancelable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accepted := &stubDatabase{}
			if err := test.requeue(accepted.repository(t), testJobID); err != nil {
				t.Fatal(err)
			}
			update := accepted.executed(test.condition)
			if len(update) != 1 || !strings.Contains(update[0].query, "attempts = 0") || !strings.Contains(update[0].query, "status = 'pending'") {
				t.Fatalf("requeue statements = %+v", update)
			}
			if (test.name == "cancel") != strings.Contains(update[0].query, "operation = 'compensate'") {
				t.Fatalf("operation change in %s", update[0].query)
			}
			running := (&stubDatabase{}).onExec("WHERE id = $1 AND", 0).onQuery(jobByID, jobRow(t, testRequest(), Running, 1))
			if err := test.requeue(running.repository(t), testJobID); !errors.Is(err, test.rejected) {
				t.Fatalf("requeueing a running job returned %v", err)
			}
			missing := (&stubDatabase{}).onExec("WHERE id = $1 AND", 0).onQuery(jobByID)
			if err := test.requeue(missing.repository(t), testJobID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("requeueing a missing job returned %v", err)
			}
			if err := test.requeue((&stubDatabase{}).repository(t), "job"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("requeueing a malformed ID returned %v", err)
			}
		})
	}
}

func (r *Repository) resumeFor(jobID string) error { return r.Resume(context.Background(), jobID) }
func (r *Repository) cancelFor(jobID string) error { return r.Cancel(context.Background(), jobID) }
