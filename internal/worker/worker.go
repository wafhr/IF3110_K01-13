package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

// TraceAttempt membungkus satu attempt setelah job dimuat dari antrean persisten.
// run menerima context worker untuk seluruh lock, batch, SQL, dan commit/rollback.
// run mengembalikan nil setelah hasil attempt dan commit sudah diketahui.
func TraceAttempt(ctx context.Context, request migration.ExecutionRequest, jobID string, attempt int, run func(context.Context) error) (attemptErr error) {
	ctx = request.ExecutionContext(ctx)
	ctx, activity := telemetry.StartConsumerActivity(ctx, request.TraceContext, "worker_attempt",
		slog.String("job_id", jobID), slog.Int("attempt", attempt))
	defer activity.End(&attemptErr)
	if jobID == "" || attempt < 1 || run == nil {
		return errors.New("worker attempt requires a job ID, positive attempt, and execution callback")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(ctx)
}

const stateUpdateTimeout = 10 * time.Second

var errWorkerStopping = errors.New("worker stopped before the attempt finished")

type Queue interface {
	Claim(ctx context.Context, workerID string, lease time.Duration) (job.Job, error)
	Heartbeat(ctx context.Context, jobID, workerID string, lease time.Duration) error
	UpdateProgress(ctx context.Context, jobID, workerID string, progress job.Progress) error
	Complete(ctx context.Context, jobID, workerID string) error
	Fail(ctx context.Context, jobID, workerID, reason string, committed bool) error
	Retry(ctx context.Context, jobID, workerID, reason string, delay time.Duration) error
	Release(ctx context.Context, jobID, workerID string) error
}

type Runner interface {
	Run(context.Context, migration.ExecutionRequest, ProgressFunc) (migration.Execution, error)
	Compensate(context.Context, migration.ExecutionRequest, ProgressFunc) (migration.Execution, error)
}

type Config struct {
	ID           string
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
	DrainTimeout time.Duration
	Retry        RetryPolicy
}

func (c Config) validate() error {
	if c.ID == "" || c.Concurrency < 1 || c.PollInterval <= 0 || c.DrainTimeout <= 0 {
		return errors.New("worker requires an ID, positive concurrency, poll interval, and drain timeout")
	}
	if c.Lease < 3*time.Second {
		return errors.New("worker lease must be at least three seconds")
	}
	return c.Retry.validate()
}

// Worker claims queued jobs and runs each attempt while renewing its lease.
type Worker struct {
	queue  Queue
	runner Runner
	config Config
}

func New(queue Queue, runner Runner, config Config) (*Worker, error) {
	if queue == nil || runner == nil {
		return nil, errors.New("worker requires a queue and a runner")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Worker{queue: queue, runner: runner, config: config}, nil
}

// Run polls until ctx is cancelled. Attempts already running get DrainTimeout
// to finish; unfinished ones are returned to the queue for another worker.
func (w *Worker) Run(ctx context.Context) error {
	attempts, stopAttempts := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAttempts()
	stopDrain := context.AfterFunc(ctx, func() {
		timer := time.AfterFunc(w.config.DrainTimeout, stopAttempts)
		context.AfterFunc(attempts, func() { timer.Stop() })
	})
	defer stopDrain()

	var slots sync.WaitGroup
	for range w.config.Concurrency {
		slots.Go(func() { w.poll(ctx, attempts) })
	}
	slots.Wait()
	return nil
}

func (w *Worker) poll(ctx, attempts context.Context) {
	for ctx.Err() == nil {
		claimed, err := w.queue.Claim(ctx, w.config.ID, w.config.Lease)
		switch {
		case err == nil:
			w.process(attempts, claimed)
			continue
		case !errors.Is(err, job.ErrNoJob) && ctx.Err() == nil:
			slog.ErrorContext(ctx, "job_claim_failed", "actor", "system", "status", "failed", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.config.PollInterval):
		}
	}
}

func (w *Worker) process(ctx context.Context, claimed job.Job) {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	heartbeat := w.keepLease(attemptCtx, claimed.ID, cancel)
	committed := false
	report := func(latest job.Progress) {
		committed = true
		if err := w.queue.UpdateProgress(attemptCtx, claimed.ID, w.config.ID, latest); errors.Is(err, job.ErrLeaseLost) {
			cancel(job.ErrLeaseLost)
		} else if err != nil {
			slog.WarnContext(attemptCtx, "job_progress_update_failed", "job_id", claimed.ID, "status", "failed", "error", err)
		}
	}
	run := w.runner.Run
	if claimed.Operation == job.Compensate {
		run = w.runner.Compensate
	}
	var execution migration.Execution
	runErr := TraceAttempt(attemptCtx, claimed.Request, claimed.ID, claimed.Attempts, func(ctx context.Context) (err error) {
		execution, err = run(ctx, claimed.Request, report)
		return err
	})
	interruption := context.Cause(attemptCtx)
	cancel(nil)
	heartbeat.Wait()
	w.settle(ctx, claimed, execution, runErr, interruption, committed)
}

// keepLease renews the lease until the attempt ends and cancels the attempt
// when another worker has taken the job over.
func (w *Worker) keepLease(ctx context.Context, jobID string, cancel context.CancelCauseFunc) *sync.WaitGroup {
	var running sync.WaitGroup
	running.Go(func() {
		ticker := time.NewTicker(w.config.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.queue.Heartbeat(ctx, jobID, w.config.ID, w.config.Lease); errors.Is(err, job.ErrLeaseLost) {
					cancel(job.ErrLeaseLost)
					return
				} else if err != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "job_heartbeat_failed", "job_id", jobID, "status", "failed", "error", err)
				}
			}
		}
	})
	return &running
}

// settle records the attempt outcome. committed tells whether the attempt
// reported a commit, so a failure after committed batches never ends as failed.
func (w *Worker) settle(ctx context.Context, claimed job.Job, execution migration.Execution, runErr, interruption error, committed bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateUpdateTimeout)
	defer cancel()
	ctx = claimed.Request.ExecutionContext(telemetry.WithLogAttrs(ctx, slog.String("job_id", claimed.ID), slog.Int("attempt", claimed.Attempts)))
	var err error
	switch {
	case errors.Is(interruption, job.ErrLeaseLost):
		slog.WarnContext(ctx, "job_lease_lost", "status", "failed")
		return
	case runErr == nil:
		err = w.queue.Complete(ctx, claimed.ID, w.config.ID)
		slog.InfoContext(ctx, "job_completed", "status", "success", "execution_status", string(execution.Status))
	case errors.Is(interruption, context.Canceled):
		err = w.queue.Release(ctx, claimed.ID, w.config.ID)
		slog.WarnContext(ctx, "job_released", "status", "failed", "reason", errWorkerStopping.Error())
	default:
		decision := w.config.Retry.Decide(runErr, claimed.Attempts)
		if decision.Retry {
			err = w.queue.Retry(ctx, claimed.ID, w.config.ID, decision.Reason, decision.Delay)
			slog.WarnContext(ctx, "job_retry_scheduled", "status", "failed", "reason", decision.Reason, "retry_delay_ms", decision.Delay.Milliseconds())
		} else {
			err = w.queue.Fail(ctx, claimed.ID, w.config.ID, decision.Reason, committed)
			slog.ErrorContext(ctx, "job_stopped", "status", "failed", "reason", decision.Reason, "operation", string(claimed.Operation))
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "job_state_update_failed", "status", "failed", "error", err)
	}
}
