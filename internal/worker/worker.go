package worker

import (
	"context"
	"errors"
	"log/slog"

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
