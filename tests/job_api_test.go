//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

func TestJobStatusAPIIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	definition := f.createMigration(t, "add audit table", "CREATE TABLE audit_events (id BIGINT PRIMARY KEY)", "DROP TABLE audit_events")
	request := f.queueAction(t, definition, migration.Apply)
	var jobID string
	if err := f.platform.QueryRow(`SELECT id::text FROM migration_jobs WHERE execution_id = $1`, request.ExecutionID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	jobsPath := "/databases/" + f.databaseID + "/jobs"

	t.Run("submission is queued and visible to editors and viewers", func(t *testing.T) {
		for _, client := range []*apiClient{f.editor, f.reader} {
			var detail struct {
				Job job.Job `json:"job"`
			}
			body := client.do(t, http.MethodGet, jobsPath+"/"+jobID, nil, http.StatusOK)
			decode(t, body, &detail)
			if detail.Job.Status != job.Pending || detail.Job.ExecutionID != request.ExecutionID || detail.Job.Name != "add audit table" ||
				detail.Job.Action != migration.Apply || detail.Job.ActorID != f.actorID {
				t.Fatalf("job detail = %+v", detail.Job)
			}
			if strings.Contains(string(body), "CREATE TABLE") {
				t.Fatal("job detail exposed the queued SQL")
			}
		}
		var list struct {
			Jobs []job.Job `json:"jobs"`
		}
		decode(t, f.reader.do(t, http.MethodGet, jobsPath+"?limit=10", nil, http.StatusOK), &list)
		if len(list.Jobs) != 1 || list.Jobs[0].ID != jobID {
			t.Fatalf("job list = %+v", list.Jobs)
		}
		f.reader.do(t, http.MethodGet, jobsPath+"?limit=0", nil, http.StatusBadRequest)
	})

	t.Run("job status follows database access", func(t *testing.T) {
		createAccessUser(t, f.platformAdmin, "outsider", false)
		outsider := f.login(t, "outsider", memberPassword)
		outsider.do(t, http.MethodGet, jobsPath+"/"+jobID, nil, http.StatusForbidden)
		outsider.do(t, http.MethodGet, jobsPath, nil, http.StatusForbidden)
		f.anonymous().do(t, http.MethodGet, jobsPath+"/"+jobID, nil, http.StatusUnauthorized)
	})

	t.Run("a job is only readable through its own database", func(t *testing.T) {
		other := registerAccessDatabase(t, f.fixture, f.platformAdmin)
		if err := access.NewService(f.platform).SetDatabasePermission(context.Background(), f.editorGroupID, other, access.ViewOnly); err != nil {
			t.Fatal(err)
		}
		f.editor.do(t, http.MethodGet, "/databases/"+other+"/jobs/"+jobID, nil, http.StatusNotFound)
		f.editor.do(t, http.MethodGet, jobsPath+"/"+newID(), nil, http.StatusNotFound)
	})

	t.Run("resubmitting the same execution keeps one job", func(t *testing.T) {
		queue, err := job.NewService(f.jobs)
		if err != nil {
			t.Fatal(err)
		}
		again, err := queue.Enqueue(context.Background(), request)
		if err != nil || again != jobID {
			t.Fatalf("resubmission returned %q, %v; want %q", again, err, jobID)
		}
		assertCount(t, f.platform, `SELECT count(*) FROM migration_jobs WHERE execution_id = $1`, 1, request.ExecutionID)
	})

	t.Run("older attempts stay reachable when new attempts arrive", func(t *testing.T) {
		insertAttempts := func(first, last int) {
			t.Helper()
			execFixture(t, f.platform, `
				INSERT INTO migration_attempts
					(job_id, execution_id, database_id, migration_id, name, revision, action, operation,
					 attempt, actor_id, sql, status, started_at, finished_at)
				SELECT $1, $2, $3, $4, 'add audit table', 1, 'apply', 'execute',
					attempt, $5, 'CREATE TABLE audit_events (id BIGINT PRIMARY KEY)', 'failed', now(), now()
				FROM generate_series($6::int, $7::int) AS attempt`,
				jobID, request.ExecutionID, f.databaseID, definition.ID, f.actorID, first, last)
		}
		attemptsPath := "/databases/" + f.databaseID + "/attempts?limit=100"
		insertAttempts(1, 102)
		var newest job.AttemptPage
		decode(t, f.reader.do(t, http.MethodGet, attemptsPath, nil, http.StatusOK), &newest)
		if len(newest.Attempts) != 100 || newest.Attempts[0].Attempt != 102 ||
			newest.NextBefore != newest.Attempts[99].Sequence {
			t.Fatalf("first page has %d attempts and cursor %d", len(newest.Attempts), newest.NextBefore)
		}

		insertAttempts(103, 103)
		var older job.AttemptPage
		decode(t, f.reader.do(t, http.MethodGet, attemptsPath+"&before="+strconv.FormatInt(newest.NextBefore, 10),
			nil, http.StatusOK), &older)
		if len(older.Attempts) != 2 || older.Attempts[0].Attempt != 2 || older.Attempts[1].Attempt != 1 || older.NextBefore != 0 {
			t.Fatalf("older page = %+v", older)
		}
	})
}
