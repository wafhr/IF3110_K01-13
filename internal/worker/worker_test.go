package worker_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

func setupWorkerTracing(t *testing.T) (*tracetest.SpanRecorder, *bytes.Buffer) {
	t.Helper()
	provider, err := telemetry.NewTracerProvider(io.Discard, "worker")
	if err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider.RegisterSpanProcessor(recorder)
	previousProvider, previousLogger := otel.GetTracerProvider(), slog.Default()
	var logs bytes.Buffer
	otel.SetTracerProvider(provider)
	slog.SetDefault(telemetry.NewLogger(&logs, "worker"))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
		slog.SetDefault(previousLogger)
	})
	return recorder, &logs
}

type workerSQLConnector struct{}

func (workerSQLConnector) Connect(context.Context) (driver.Conn, error) { return workerSQLConn{}, nil }
func (workerSQLConnector) Driver() driver.Driver                        { return workerSQLDriver{} }

type workerSQLDriver struct{}

func (workerSQLDriver) Open(string) (driver.Conn, error) { return workerSQLConn{}, nil }

type workerSQLConn struct{}

func (workerSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unsupported prepare")
}
func (workerSQLConn) Begin() (driver.Tx, error) { return nil, errors.New("unsupported transaction") }
func (workerSQLConn) Close() error              { return nil }
func (workerSQLConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func TestTraceAttemptPreservesProducerParentAcrossRetryAndSQLScopes(t *testing.T) {
	recorder, logs := setupWorkerTracing(t)
	source, cancelRequest := context.WithCancel(context.Background())
	source, parent := otel.Tracer("test-http").Start(source, "request")
	producer, enqueue := telemetry.StartProducerActivity(source, "enqueue")
	request := migration.ExecutionRequest{
		ExecutionID: "execution-1", ActorID: "actor-1",
		Plan:         migration.ActionPlan{DatabaseID: "database-1", MigrationID: "migration-1", Revision: 3, Action: migration.Apply, SQL: "UPDATE private_table SET private_value = 'fixture-private-secret'"},
		TraceContext: telemetry.CaptureTraceContext(producer),
	}
	producerID := trace.SpanContextFromContext(producer).SpanID()
	var enqueueErr error
	enqueue.End(&enqueueErr)
	parent.End()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var restored migration.ExecutionRequest
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	db := sql.OpenDB(telemetry.SQLConnector(workerSQLConnector{}, "postgresql", "target", "target-db"))
	defer db.Close()
	failure := errors.New("fixture-private-error")
	for attempt := 1; attempt <= 2; attempt++ {
		err := worker.TraceAttempt(context.Background(), restored, "job-1", attempt, func(ctx context.Context) (runErr error) {
			if ctx.Err() != nil {
				t.Fatal("worker mewarisi pembatalan request")
			}
			ctx, lock := telemetry.StartActivity(ctx, "worker_lock")
			defer lock.End(&runErr)
			ctx, batch := telemetry.StartActivity(ctx, "worker_batch", slog.Int("batch", 7))
			defer batch.End(&runErr)
			if attempt == 1 {
				return failure
			}
			_, runErr = db.ExecContext(ctx, restored.Plan.SQL)
			return runErr
		})
		if (attempt == 1 && err != failure) || (attempt == 2 && err != nil) {
			t.Fatal("hasil callback attempt berubah")
		}
	}
	consumers := 0
	batchIDs := make(map[trace.SpanID]bool)
	for _, span := range recorder.Ended() {
		if span.Name() == "worker_batch" {
			batchIDs[span.SpanContext().SpanID()] = true
		}
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "worker_attempt" {
			consumers++
			if span.SpanKind() != trace.SpanKindConsumer || span.Parent().SpanID() != producerID || !span.Parent().IsRemote() || span.SpanContext().TraceID() != parent.SpanContext().TraceID() {
				t.Fatal("parent, kind, atau trace retry salah")
			}
		}
		if span.Name() == "db.exec" && !batchIDs[span.Parent().SpanID()] {
			t.Fatal("span SQL tidak mengikuti scope batch")
		}
	}
	if consumers != 2 {
		t.Fatal("jumlah span attempt tidak sesuai retry")
	}
	decoder := json.NewDecoder(strings.NewReader(logs.String()))
	completed, sqlSeen := 0, false
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if record["event"] == "worker_attempt_completed" {
			completed++
			if record["actor"] != restored.ActorID || record["execution_id"] != restored.ExecutionID || record["job_id"] != "job-1" || record["database_id"] != restored.Plan.DatabaseID {
				t.Fatal("metadata attempt hilang")
			}
			if (record["status"] == "failed") != (record["attempt"] == float64(1)) {
				t.Fatal("hasil retry tercatat salah")
			}
			if _, exists := record["batch"]; exists {
				t.Fatal("metadata batch bocor ke scope attempt")
			}
		}
		if record["event"] == "sql_exec_completed" {
			sqlSeen = true
			if record["attempt"] != float64(2) || record["batch"] != float64(7) || record["migration_id"] != restored.Plan.MigrationID || record["job_id"] != "job-1" || record["trace_id"] != parent.SpanContext().TraceID().String() {
				t.Fatal("korelasi SQL, batch, dan job hilang")
			}
		}
	}
	if completed != 2 || !sqlSeen {
		t.Fatal("log attempt atau SQL tidak lengkap")
	}
	if strings.Contains(logs.String(), "fixture-private-") {
		t.Fatal("payload atau error mentah muncul pada log worker")
	}
}

func TestTraceAttemptSupportsLegacyPayloadAndRejectsCanceledWorker(t *testing.T) {
	recorder, _ := setupWorkerTracing(t)
	ctx, poller := otel.Tracer("poller").Start(context.Background(), "poller")
	defer poller.End()
	for _, carrier := range []telemetry.TraceContext{{}, {TraceParent: "invalid"}} {
		called := false
		err := worker.TraceAttempt(ctx, migration.ExecutionRequest{TraceContext: carrier}, "legacy-job", 1, func(ctx context.Context) error {
			called = true
			if trace.SpanContextFromContext(ctx).TraceID() == poller.SpanContext().TraceID() {
				t.Fatal("payload lama memakai trace poller")
			}
			return nil
		})
		if err != nil || !called {
			t.Fatal("payload tanpa trace valid tidak dapat diproses")
		}
	}
	if len(recorder.Ended()) != 2 {
		t.Fatal("span untuk payload lama tidak diakhiri")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := worker.TraceAttempt(canceled, migration.ExecutionRequest{}, "job", 1, func(context.Context) error {
		t.Fatal("callback berjalan setelah worker dibatalkan")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pembatalan worker tidak diteruskan")
	}
}

func TestTraceAttemptReportsPanicAndInvalidInputs(t *testing.T) {
	recorder, logs := setupWorkerTracing(t)
	for _, test := range []struct {
		job     string
		attempt int
		run     func(context.Context) error
	}{
		{"", 1, func(context.Context) error {
			t.Fatal("job kosong diproses")
			return nil
		}},
		{"job", 0, func(context.Context) error {
			t.Fatal("attempt nol diproses")
			return nil
		}},
		{"job", 1, nil},
	} {
		if err := worker.TraceAttempt(context.Background(), migration.ExecutionRequest{}, test.job, test.attempt, test.run); err == nil {
			t.Fatal("input attempt tidak valid diterima")
		}
	}
	func() {
		defer func() {
			if recover() != "fixture-private-panic" {
				t.Fatal("panic callback berubah atau ditelan")
			}
		}()
		_ = worker.TraceAttempt(context.Background(), migration.ExecutionRequest{}, "job", 1, func(context.Context) error { panic("fixture-private-panic") })
	}()
	if len(recorder.Ended()) != 4 {
		t.Fatal("span kegagalan attempt tidak diakhiri")
	}
	spans, _ := json.Marshal(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()))
	if strings.Contains(logs.String(), "fixture-private-panic") || strings.Contains(string(spans), "fixture-private-panic") {
		t.Fatal("nilai panic mentah muncul pada telemetry")
	}
}

type settlement struct {
	jobID, kind, reason string
	delay               time.Duration
}

type fakeQueue struct {
	mu        sync.Mutex
	pending   []job.Job
	claimErrs []error
	heartbeat error
	settled   chan settlement
}

func newFakeQueue(jobs ...job.Job) *fakeQueue {
	return &fakeQueue{pending: jobs, settled: make(chan settlement, len(jobs)+1)}
}

func (q *fakeQueue) Claim(context.Context, string, time.Duration) (job.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.claimErrs) > 0 {
		err := q.claimErrs[0]
		q.claimErrs = q.claimErrs[1:]
		return job.Job{}, err
	}
	if len(q.pending) == 0 {
		return job.Job{}, job.ErrNoJob
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *fakeQueue) Heartbeat(context.Context, string, string, time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.heartbeat
}

func (q *fakeQueue) Complete(_ context.Context, jobID, _ string) error {
	q.settled <- settlement{jobID: jobID, kind: "complete"}
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, jobID, _, reason string) error {
	q.settled <- settlement{jobID: jobID, kind: "fail", reason: reason}
	return nil
}

func (q *fakeQueue) Release(_ context.Context, jobID, _ string) error {
	q.settled <- settlement{jobID: jobID, kind: "release"}
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, jobID, _, reason string, delay time.Duration) error {
	q.settled <- settlement{jobID: jobID, kind: "retry", reason: reason, delay: delay}
	return nil
}

type runnerFunc func(context.Context, migration.ExecutionRequest) (migration.Execution, error)

func (f runnerFunc) Run(ctx context.Context, request migration.ExecutionRequest) (migration.Execution, error) {
	return f(ctx, request)
}

func claimedJob(id string, attempts int) job.Job {
	return job.Job{ID: id, Status: job.Running, Attempts: attempts, Request: migration.ExecutionRequest{
		ExecutionID: "execution-" + id, Plan: migration.ActionPlan{DatabaseID: "database-" + id, Action: migration.Apply, SQL: "SELECT 1"},
	}}
}

func testConfig() worker.Config {
	return worker.Config{
		ID: "worker-test", Concurrency: 1, PollInterval: 10 * time.Millisecond, Lease: 3 * time.Second,
		DrainTimeout: time.Second, Retry: worker.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Second, MaxDelay: 5 * time.Second},
	}
}

func startWorker(t *testing.T, queue worker.Queue, runner worker.Runner, config worker.Config) (stop func()) {
	t.Helper()
	consumer, err := worker.New(queue, runner, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumer.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func nextSettlement(t *testing.T, queue *fakeQueue) settlement {
	t.Helper()
	select {
	case settled := <-queue.settled:
		return settled
	case <-time.After(5 * time.Second):
		t.Fatal("job was not settled")
		return settlement{}
	}
}

func TestNewWorkerValidatesConfiguration(t *testing.T) {
	runner := runnerFunc(func(context.Context, migration.ExecutionRequest) (migration.Execution, error) {
		return migration.Execution{}, nil
	})
	for name, change := range map[string]func(*worker.Config){
		"missing ID":           func(c *worker.Config) { c.ID = "" },
		"no concurrency":       func(c *worker.Config) { c.Concurrency = 0 },
		"no poll interval":     func(c *worker.Config) { c.PollInterval = 0 },
		"short lease":          func(c *worker.Config) { c.Lease = time.Second },
		"no drain timeout":     func(c *worker.Config) { c.DrainTimeout = 0 },
		"invalid retry policy": func(c *worker.Config) { c.Retry.MaxAttempts = 0 },
	} {
		config := testConfig()
		change(&config)
		if _, err := worker.New(newFakeQueue(), runner, config); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := worker.New(nil, runner, testConfig()); err == nil {
		t.Error("nil queue accepted")
	}
}

func TestWorkerCompletesSuccessfulAttempt(t *testing.T) {
	queue := newFakeQueue(claimedJob("a", 1))
	queue.claimErrs = []error{errors.New("platform database restarting")}
	var received migration.ExecutionRequest
	startWorker(t, queue, runnerFunc(func(_ context.Context, request migration.ExecutionRequest) (migration.Execution, error) {
		received = request
		return migration.Execution{ID: request.ExecutionID, Status: migration.Success}, nil
	}), testConfig())
	if settled := nextSettlement(t, queue); settled.kind != "complete" || settled.jobID != "a" || received.ExecutionID != "execution-a" {
		t.Fatalf("settlement = %+v, request = %+v", settled, received)
	}
}

func TestWorkerSettlesFailedAttempts(t *testing.T) {
	tests := []struct {
		name      string
		attempts  int
		err       error
		wantKind  string
		wantDelay time.Duration
	}{
		{name: "permanent failure", attempts: 1, err: migration.ErrApprovalRequired, wantKind: "fail"},
		{name: "transient failure is retried", attempts: 1, err: errors.New("connection reset"), wantKind: "retry", wantDelay: time.Second},
		{name: "transient failure at the attempt limit", attempts: 3, err: errors.New("connection reset"), wantKind: "fail"},
		{name: "uncertain commit waits for evidence", attempts: 7, err: worker.ErrCommitUncertain, wantKind: "retry", wantDelay: 5 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queue := newFakeQueue(claimedJob("a", test.attempts))
			startWorker(t, queue, runnerFunc(func(context.Context, migration.ExecutionRequest) (migration.Execution, error) {
				return migration.Execution{}, test.err
			}), testConfig())
			settled := nextSettlement(t, queue)
			if settled.kind != test.wantKind || settled.delay != test.wantDelay || settled.reason == "" {
				t.Fatalf("settlement = %+v", settled)
			}
		})
	}
}

func TestWorkerRunsJobsConcurrently(t *testing.T) {
	queue := newFakeQueue(claimedJob("a", 1), claimedJob("b", 1))
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	config := testConfig()
	config.Concurrency = 2
	startWorker(t, queue, runnerFunc(func(ctx context.Context, _ migration.ExecutionRequest) (migration.Execution, error) {
		arrived.Done()
		select {
		case <-both:
			return migration.Execution{}, nil
		case <-ctx.Done():
			return migration.Execution{}, ctx.Err()
		}
	}), config)
	for range 2 {
		if settled := nextSettlement(t, queue); settled.kind != "complete" {
			t.Fatalf("settlement = %+v", settled)
		}
	}
}

func TestWorkerShutdown(t *testing.T) {
	t.Run("attempt finishing within the drain timeout completes", func(t *testing.T) {
		queue := newFakeQueue(claimedJob("a", 1))
		started, release := make(chan struct{}), make(chan struct{})
		stop := startWorker(t, queue, runnerFunc(func(ctx context.Context, _ migration.ExecutionRequest) (migration.Execution, error) {
			close(started)
			<-release
			return migration.Execution{}, ctx.Err()
		}), testConfig())
		<-started
		go stop()
		time.Sleep(50 * time.Millisecond)
		close(release)
		if settled := nextSettlement(t, queue); settled.kind != "complete" {
			t.Fatalf("settlement = %+v", settled)
		}
	})
	t.Run("attempt exceeding the drain timeout is released", func(t *testing.T) {
		queue := newFakeQueue(claimedJob("a", 1))
		started := make(chan struct{})
		config := testConfig()
		config.DrainTimeout = 50 * time.Millisecond
		stop := startWorker(t, queue, runnerFunc(func(ctx context.Context, _ migration.ExecutionRequest) (migration.Execution, error) {
			close(started)
			<-ctx.Done()
			return migration.Execution{}, ctx.Err()
		}), config)
		<-started
		stop()
		if settled := nextSettlement(t, queue); settled.kind != "release" {
			t.Fatalf("settlement = %+v", settled)
		}
	})
}

func TestWorkerAbandonsAttemptAfterLosingLease(t *testing.T) {
	queue := newFakeQueue(claimedJob("a", 1))
	queue.heartbeat = job.ErrLeaseLost
	interrupted := make(chan error, 1)
	stop := startWorker(t, queue, runnerFunc(func(ctx context.Context, _ migration.ExecutionRequest) (migration.Execution, error) {
		<-ctx.Done()
		interrupted <- context.Cause(ctx)
		return migration.Execution{}, ctx.Err()
	}), testConfig())
	select {
	case cause := <-interrupted:
		if !errors.Is(cause, job.ErrLeaseLost) {
			t.Fatalf("attempt cancelled by %v", cause)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attempt kept running after the lease was lost")
	}
	stop()
	select {
	case settled := <-queue.settled:
		t.Fatalf("worker changed a job it no longer owns: %+v", settled)
	default:
	}
}
