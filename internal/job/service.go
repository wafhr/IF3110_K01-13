package job

import (
	"context"
	"errors"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

type Store interface {
	Enqueue(context.Context, migration.ExecutionRequest) (Job, error)
	Get(context.Context, string) (Job, error)
	ListByDatabase(context.Context, string, int) ([]Job, error)
	Resume(context.Context, string) error
	Cancel(context.Context, string) error
}

// Service is the queue used by migration submissions and the read model of the
// job status API.
type Service struct {
	store Store
}

var _ migration.ActionQueue = (*Service)(nil)

func NewService(store Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("job service requires a store")
	}
	return &Service{store: store}, nil
}

// Enqueue returns the job ID only after the request is stored.
func (s *Service) Enqueue(ctx context.Context, request migration.ExecutionRequest) (string, error) {
	job, err := s.store.Enqueue(ctx, request)
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// Job returns a job only through the database registration it was queued for.
func (s *Service) Job(ctx context.Context, databaseID, jobID string) (Job, error) {
	job, err := s.store.Get(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if !strings.EqualFold(job.DatabaseID, databaseID) {
		return Job{}, ErrNotFound
	}
	return job, nil
}

// Resume continues a stopped batch migration or compensation of the given
// database. The worker revalidates permission and approval before every batch.
func (s *Service) Resume(ctx context.Context, databaseID, jobID string) (Job, error) {
	if _, err := s.Job(ctx, databaseID, jobID); err != nil {
		return Job{}, err
	}
	if err := s.store.Resume(ctx, jobID); err != nil {
		return Job{}, err
	}
	return s.Job(ctx, databaseID, jobID)
}

// Cancel reverts the committed batches of a partial apply or forward with the
// down SQL approved in its plan. An irreversible migration cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, databaseID, jobID string) (Job, error) {
	job, err := s.Job(ctx, databaseID, jobID)
	if err != nil {
		return Job{}, err
	}
	if job.Request.Plan.Batch.Enabled() && job.Request.Plan.Action != migration.Rollback && job.Request.Plan.CompensationSQL == "" {
		return Job{}, ErrIrreversible
	}
	if err := s.store.Cancel(ctx, jobID); err != nil {
		return Job{}, err
	}
	return s.Job(ctx, databaseID, jobID)
}

func (s *Service) Jobs(ctx context.Context, databaseID string, limit int) ([]Job, error) {
	return s.store.ListByDatabase(ctx, databaseID, limit)
}
