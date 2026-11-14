package job

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/migration"
)

const otherDatabaseID = "10000000-0000-4000-8000-000000000099"

type fakeStore struct {
	jobs        map[string]Job
	err         error
	enqueued    []migration.ExecutionRequest
	listedFor   string
	listedLimit int
}

func newFakeStore(jobs ...Job) *fakeStore {
	store := &fakeStore{jobs: map[string]Job{}}
	for _, job := range jobs {
		store.jobs[job.ID] = job
	}
	return store
}

func (s *fakeStore) Enqueue(_ context.Context, request migration.ExecutionRequest) (Job, error) {
	s.enqueued = append(s.enqueued, request)
	if s.err != nil {
		return Job{}, s.err
	}
	return Job{ID: testJobID, Request: request}, nil
}

func (s *fakeStore) Get(_ context.Context, jobID string) (Job, error) {
	if s.err != nil {
		return Job{}, s.err
	}
	job, ok := s.jobs[jobID]
	if !ok {
		return Job{}, ErrNotFound
	}
	return job, nil
}

func (s *fakeStore) ListByDatabase(_ context.Context, databaseID string, limit int) ([]Job, error) {
	s.listedFor, s.listedLimit = databaseID, limit
	if s.err != nil {
		return nil, s.err
	}
	jobs := make([]Job, 0)
	for _, job := range s.jobs {
		if job.DatabaseID == databaseID {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

func queuedJob() Job {
	job := Job{ID: testJobID, Status: Pending, Request: testRequest(), CreatedAt: testCreatedAt, AvailableAt: testCreatedAt}
	job.describe()
	return job
}

func newTestService(t *testing.T, store Store) *Service {
	t.Helper()
	service, err := NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNewServiceRequiresStore(t *testing.T) {
	if _, err := NewService(nil); err == nil {
		t.Fatal("service accepted a nil store")
	}
}

func TestServiceEnqueueReturnsStoredJobID(t *testing.T) {
	store := newFakeStore()
	jobID, err := newTestService(t, store).Enqueue(context.Background(), testRequest())
	if err != nil || jobID != testJobID || len(store.enqueued) != 1 || store.enqueued[0] != testRequest() {
		t.Fatalf("enqueue = %q, %v; stored %+v", jobID, err, store.enqueued)
	}
	store.err = ErrInvalidJob
	if jobID, err := newTestService(t, store).Enqueue(context.Background(), testRequest()); jobID != "" || !errors.Is(err, ErrInvalidJob) {
		t.Fatalf("failed enqueue = %q, %v", jobID, err)
	}
}

func TestServiceJobIsScopedToItsDatabase(t *testing.T) {
	service := newTestService(t, newFakeStore(queuedJob()))
	job, err := service.Job(context.Background(), testDatabaseID, testJobID)
	if err != nil || job.ID != testJobID {
		t.Fatalf("job = %+v, %v", job, err)
	}
	if job, err := service.Job(context.Background(), strings.ToUpper(testDatabaseID), testJobID); err != nil || job.ID != testJobID {
		t.Fatalf("job through an upper-case database ID = %+v, %v", job, err)
	}
	if _, err := service.Job(context.Background(), otherDatabaseID, testJobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job through another database returned %v", err)
	}
	if _, err := service.Job(context.Background(), testDatabaseID, "10000000-0000-4000-8000-0000000000aa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job returned %v", err)
	}
}

func TestServiceJobsListsTheRequestedDatabase(t *testing.T) {
	store := newFakeStore(queuedJob())
	jobs, err := newTestService(t, store).Jobs(context.Background(), testDatabaseID, 10)
	if err != nil || len(jobs) != 1 || store.listedFor != testDatabaseID || store.listedLimit != 10 {
		t.Fatalf("jobs = %+v, %v; listed %q/%d", jobs, err, store.listedFor, store.listedLimit)
	}
}
