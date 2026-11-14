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

func (s *Service) Jobs(ctx context.Context, databaseID string, limit int) ([]Job, error) {
	return s.store.ListByDatabase(ctx, databaseID, limit)
}
