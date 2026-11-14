//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

const jobLease = time.Minute

func TestJobQueueIntegration(t *testing.T) {
	f := newFixture(t)
	queue := newJobQueue(t, f)
	ctx := context.Background()

	t.Run("enqueue persists the request and ignores redelivery", func(t *testing.T) {
		request := queuedRequest(newID())
		first, err := queue.Enqueue(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		redelivered := request
		redelivered.TraceContext = telemetry.TraceContext{}
		second, err := queue.Enqueue(ctx, redelivered)
		if err != nil || second.ID != first.ID {
			t.Fatalf("redelivery returned job %q, %v; want %q", second.ID, err, first.ID)
		}
		changed := request
		changed.Plan.SQL = "DROP TABLE accounts"
		if _, err := queue.Enqueue(ctx, changed); !errors.Is(err, job.ErrConflict) {
			t.Fatalf("changed snapshot returned %v", err)
		}
		stored, err := queue.Get(ctx, first.ID)
		if err != nil || stored.Status != job.Pending || stored.Request != request {
			t.Fatalf("stored job = %+v, %v", stored, err)
		}
		assertCount(t, f.platform, `SELECT count(*) FROM migration_jobs WHERE execution_id = $1`, 1, request.ExecutionID)
		completeAll(t, f)
	})

	t.Run("jobs of one database run in order while other databases proceed", func(t *testing.T) {
		first, second := newID(), newID()
		a1 := enqueue(t, queue, first)
		a2 := enqueue(t, queue, first)
		b1 := enqueue(t, queue, second)

		claimed := claim(t, queue, "worker-a")
		if claimed.ID != a1.ID {
			t.Fatalf("first claim = %s, want %s", claimed.ID, a1.ID)
		}
		if parallel := claim(t, queue, "worker-b"); parallel.ID != b1.ID {
			t.Fatalf("second claim = %s, want job of the other database", parallel.ID)
		}
		expectEmpty(t, queue, "worker-c")
		if err := queue.Complete(ctx, a1.ID, "worker-a"); err != nil {
			t.Fatal(err)
		}
		if next := claim(t, queue, "worker-c"); next.ID != a2.ID {
			t.Fatalf("claim after completion = %s, want %s", next.ID, a2.ID)
		}
		completeAll(t, f)
	})

	t.Run("concurrent workers never claim the same job twice", func(t *testing.T) {
		const jobs, workers = 24, 6
		for range jobs {
			enqueue(t, queue, newID())
		}
		var mu sync.Mutex
		owners := map[string]string{}
		var group sync.WaitGroup
		for w := range workers {
			group.Add(1)
			go func(workerID string) {
				defer group.Done()
				for {
					claimed, err := queue.Claim(ctx, workerID, jobLease)
					if errors.Is(err, job.ErrNoJob) {
						return
					}
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					if previous, taken := owners[claimed.ID]; taken {
						t.Errorf("job %s claimed by %s and %s", claimed.ID, previous, workerID)
					}
					owners[claimed.ID] = workerID
					mu.Unlock()
				}
			}(fmt.Sprintf("worker-%d", w))
		}
		group.Wait()
		if len(owners) != jobs {
			t.Fatalf("claimed %d jobs, want %d", len(owners), jobs)
		}
		completeAll(t, f)
	})

	t.Run("expired lease is reclaimed and the previous owner is fenced", func(t *testing.T) {
		queued := enqueue(t, queue, newID())
		claimed := claim(t, queue, "worker-a")
		if err := queue.Heartbeat(ctx, claimed.ID, "worker-a", jobLease); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, queue, "worker-b")
		execFixture(t, f.platform, `UPDATE migration_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, queued.ID)

		reclaimed := claim(t, queue, "worker-b")
		if reclaimed.ID != queued.ID || reclaimed.Attempts != 2 || !reclaimed.StartedAt.Equal(*claimed.StartedAt) {
			t.Fatalf("reclaimed job = %+v", reclaimed)
		}
		for _, stale := range []error{
			queue.Heartbeat(ctx, queued.ID, "worker-a", jobLease),
			queue.Complete(ctx, queued.ID, "worker-a"),
			queue.Fail(ctx, queued.ID, "worker-a", "late failure", false),
		} {
			if !errors.Is(stale, job.ErrLeaseLost) {
				t.Fatalf("stale worker transition returned %v", stale)
			}
		}
		if err := queue.Complete(ctx, queued.ID, "worker-b"); err != nil {
			t.Fatal(err)
		}
		done, err := queue.Get(ctx, queued.ID)
		if err != nil || done.Status != job.Succeeded || done.FinishedAt == nil {
			t.Fatalf("completed job = %+v, %v", done, err)
		}
	})

	t.Run("retry waits for its delay and keeps the attempt count", func(t *testing.T) {
		queued := enqueue(t, queue, newID())
		claim(t, queue, "worker-a")
		if err := queue.Retry(ctx, queued.ID, "worker-a", "connection reset", time.Hour); err != nil {
			t.Fatal(err)
		}
		expectEmpty(t, queue, "worker-a")
		waiting, err := queue.Get(ctx, queued.ID)
		if err != nil || waiting.Status != job.Pending || waiting.LastError != "connection reset" || waiting.Attempts != 1 {
			t.Fatalf("retried job = %+v, %v", waiting, err)
		}
		execFixture(t, f.platform, `UPDATE migration_jobs SET available_at = now() WHERE id = $1`, queued.ID)
		if retried := claim(t, queue, "worker-b"); retried.ID != queued.ID || retried.Attempts != 2 {
			t.Fatalf("retried claim = %+v", retried)
		}
		if err := queue.Fail(ctx, queued.ID, "worker-b", "syntax error", false); err != nil {
			t.Fatal(err)
		}
		failed, err := queue.Get(ctx, queued.ID)
		if err != nil || failed.Status != job.Failed || failed.LastError != "syntax error" || failed.FinishedAt == nil {
			t.Fatalf("failed job = %+v, %v", failed, err)
		}
	})

	t.Run("partial and failed jobs decide whether later jobs may run", func(t *testing.T) {
		databaseID := newID()
		partial := enqueue(t, queue, databaseID)
		later := enqueue(t, queue, databaseID)
		claim(t, queue, "worker-a")
		execFixture(t, f.platform, `UPDATE migration_jobs SET status = 'partial', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`, partial.ID)
		expectEmpty(t, queue, "worker-b")

		execFixture(t, f.platform, `UPDATE migration_jobs SET status = 'failed', finished_at = now() WHERE id = $1`, partial.ID)
		if next := claim(t, queue, "worker-b"); next.ID != later.ID {
			t.Fatalf("claim after failure = %s, want %s", next.ID, later.ID)
		}
		completeAll(t, f)
	})

	t.Run("a failure after a committed change stops the job as partial", func(t *testing.T) {
		databaseID := newID()
		stopped := enqueue(t, queue, databaseID)
		later := enqueue(t, queue, databaseID)
		claim(t, queue, "worker-a")
		if err := queue.Fail(ctx, stopped.ID, "worker-a", "division by zero", true); err != nil {
			t.Fatal(err)
		}
		partial, err := queue.Get(ctx, stopped.ID)
		if err != nil || partial.Status != job.Partial || partial.FinishedAt != nil {
			t.Fatalf("stopped job = %+v, %v", partial, err)
		}
		expectEmpty(t, queue, "worker-b")
		if err := queue.Cancel(ctx, stopped.ID); err != nil {
			t.Fatal(err)
		}
		compensation := claim(t, queue, "worker-b")
		if compensation.ID != stopped.ID || compensation.Operation != job.Compensate {
			t.Fatalf("claimed compensation = %+v", compensation)
		}
		if err := queue.Fail(ctx, stopped.ID, "worker-b", "check constraint violated", false); err != nil {
			t.Fatal(err)
		}
		if reverting, err := queue.Get(ctx, stopped.ID); err != nil || reverting.Status != job.PartiallyReverted {
			t.Fatalf("failed compensation = %+v, %v", reverting, err)
		}
		if err := queue.Resume(ctx, stopped.ID); err != nil {
			t.Fatal(err)
		}
		claim(t, queue, "worker-c")
		if err := queue.Complete(ctx, stopped.ID, "worker-c"); err != nil {
			t.Fatal(err)
		}
		if reverted, err := queue.Get(ctx, stopped.ID); err != nil || reverted.Status != job.Reverted {
			t.Fatalf("finished compensation = %+v, %v", reverted, err)
		}
		if next := claim(t, queue, "worker-c"); next.ID != later.ID {
			t.Fatalf("claim after compensation = %s, want %s", next.ID, later.ID)
		}
		completeAll(t, f)
	})

	t.Run("jobs survive a new repository instance", func(t *testing.T) {
		queued := enqueue(t, queue, newID())
		restarted := newJobQueue(t, f)
		claimed := claim(t, restarted, "worker-after-restart")
		if claimed.ID != queued.ID || claimed.Request.TraceContext != queued.Request.TraceContext {
			t.Fatalf("job after restart = %+v", claimed)
		}
		completeAll(t, f)
	})
}

func newJobQueue(t *testing.T, f *fixture) *job.Repository {
	t.Helper()
	queue, err := job.NewRepository(f.platform)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize job queue: %v", err)
	}
	return queue
}

func queuedRequest(databaseID string) migration.ExecutionRequest {
	return migration.ExecutionRequest{
		ExecutionID: newID(), ApprovalID: newID(), ActorID: newID(),
		Plan: migration.ActionPlan{
			DatabaseID: databaseID, MigrationID: newID(), Revision: 1, Name: "add accounts",
			Action: migration.Apply, SQL: "CREATE TABLE accounts (id BIGINT PRIMARY KEY)",
		},
		TraceContext: telemetry.TraceContext{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
}

func enqueue(t *testing.T, queue *job.Repository, databaseID string) job.Job {
	t.Helper()
	queued, err := queue.Enqueue(context.Background(), queuedRequest(databaseID))
	if err != nil {
		t.Fatal(err)
	}
	return queued
}

func claim(t *testing.T, queue *job.Repository, workerID string) job.Job {
	t.Helper()
	claimed, err := queue.Claim(context.Background(), workerID, jobLease)
	if err != nil {
		t.Fatalf("%s claim: %v", workerID, err)
	}
	if claimed.Status != job.Running {
		t.Fatalf("claimed job status = %s", claimed.Status)
	}
	return claimed
}

func expectEmpty(t *testing.T, queue *job.Repository, workerID string) {
	t.Helper()
	if claimed, err := queue.Claim(context.Background(), workerID, jobLease); !errors.Is(err, job.ErrNoJob) {
		t.Fatalf("%s claimed %s (%v), want an empty queue", workerID, claimed.ID, err)
	}
}

func completeAll(t *testing.T, f *fixture) {
	t.Helper()
	execFixture(t, f.platform, `UPDATE migration_jobs SET status = 'succeeded', lease_owner = NULL, lease_expires_at = NULL, finished_at = now() WHERE NOT status IN ('succeeded', 'failed')`)
}

func newID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	id[6], id[8] = (id[6]&0x0f)|0x40, (id[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}
