# Migration execution

Every migration action (apply, rollback, and forward) is published as a job
and executed asynchronously by the worker. The submission API returns `202`
with the job ID and execution ID as soon as the job is stored; clients follow
its progress through the job status API.

## Job queue

`internal/job` stores the queue in the `migration_jobs` table of the platform
database, so jobs survive server and worker restarts.

| Column | Purpose |
|---|---|
| `id`, `queue_position` | Job identity and submission order |
| `execution_id` | Unique execution identity; resubmitting it returns the stored job |
| `database_id` | Database registration the job runs against |
| `payload` | Complete `migration.ExecutionRequest`, including the reviewed plan, SQL, and W3C trace context |
| `status` | `pending`, `running`, `partial`, `succeeded`, or `failed` |
| `attempts`, `last_error`, `available_at` | Attempt count, last failure reason, and next retry time |
| `committed_batches`, `committed_rows` | Committed progress of batch migrations |
| `lease_owner`, `lease_expires_at` | Worker currently holding a running job |

Queue rules:

- `Claim` takes the oldest runnable job with `FOR UPDATE SKIP LOCKED`, so
  concurrent workers never receive the same job.
- A job is runnable only when no earlier job of the same database is still
  `pending`, `running`, or `partial`. Each database therefore runs one
  migration at a time in submission order, different databases run in
  parallel, and a partial migration holds later jobs until it is resolved.
- A running job whose lease expired is claimed again by another worker. State
  changes (`Heartbeat`, `Complete`, `Fail`, `Retry`) succeed only for the
  worker that still holds the lease; a worker that lost it receives
  `ErrLeaseLost`.
- `Retry` returns a job to the queue after a delay and keeps its attempt count.
- Lease and retry times use the database clock, so worker hosts do not need
  synchronized clocks.

## Worker

`cmd/worker` runs `internal/worker`. Start it next to the server with the same
`.env`; several worker processes can run at once. The server must have started
once so the access-control schema exists.

The server and the worker build the migration service with
`app.NewMigrationService`, so a submission and its execution are validated by
the same permission and approval providers. The approval provider of the
workflow module is added there once, for both processes.

```sh
go run ./cmd/worker
```

| Variable | Default | Purpose |
|---|---|---|
| `WORKER_ID` | host name and process ID | Lease owner recorded on running jobs |
| `WORKER_CONCURRENCY` | `4` | Jobs processed in parallel by one process |
| `WORKER_POLL_INTERVAL` | `1s` | Wait between claims when the queue is empty |
| `WORKER_LEASE` | `30s` | Job lease, renewed every third of its length; at least `3s` |
| `WORKER_MAX_ATTEMPTS` | `5` | Attempts before a transient failure becomes final |
| `SHUTDOWN_TIMEOUT` | `10s` | Time running attempts get to finish after a stop signal |

### Attempt

Each claimed job runs as one attempt inside `worker.TraceAttempt`, so the
attempt span continues the trace of the API request that queued it.

1. Open the target through the registry. Only PostgreSQL targets are accepted.
2. Take `pg_advisory_lock(MigrationLockKey(databaseID))` on a dedicated
   connection. The lock serializes migrations on one physical database across
   all workers and registration aliases.
3. Begin a `READ COMMITTED` transaction and call `PrepareExecution`, which
   revalidates the actor's permission, approval, plan snapshot, and history.
4. If the target already holds commit evidence for the execution ID, complete
   the job without running SQL again. Duplicate deliveries never commit twice.
5. Otherwise run the SQL, record history in the same transaction, and commit.
6. Release the lock, then mark the job `succeeded`, schedule a retry, or mark
   it `failed`.

### Failures and retries

| Failure | Outcome |
|---|---|
| Rejected revalidation (permission, approval, outdated plan), SQL errors such as syntax or constraint violations, unsupported target | `failed` immediately |
| Connection loss, serialization failure, deadlock, lock timeout, server shutdown, platform storage errors | Retried with exponential backoff (1s doubling up to 1m) until `WORKER_MAX_ATTEMPTS` |
| Commit sent but no response received | Retried without an attempt limit; the next attempt finds the commit evidence or confirms nothing was committed |

A failed attempt rolls back its transaction, so neither the SQL nor the history
entry is kept and the active migration position does not change. The job
keeps `last_error` with the SQLSTATE and primary message only; the error detail
is written to the application log instead.

While a job is pending a retry, later jobs of the same database stay queued,
so a migration whose result is not yet certain blocks the next one.

### Worker failure and lock recovery

- The advisory lock is a PostgreSQL session lock. If a worker process dies,
  PostgreSQL ends its sessions and releases the lock; no manual unlock is needed.
- The job's lease is no longer renewed and expires after `WORKER_LEASE`.
  Another worker then claims it with a new attempt number.
- That attempt calls `PrepareExecution`. If the dead worker had committed, the
  evidence completes the job; otherwise the SQL runs once.
- A worker that loses its lease while still running stops its attempt and
  leaves the job to the new owner; queue updates from the old owner are rejected.
- On `SIGINT`/`SIGTERM` a worker stops claiming, lets running attempts finish
  within `SHUTDOWN_TIMEOUT`, and returns unfinished jobs to the queue without
  counting the interrupted attempt against `WORKER_MAX_ATTEMPTS`.

## Job status API

Both endpoints require a signed-in user with at least View Only access to the
database. A job is only readable through the database it was queued for.

| Method and path | Response |
|---|---|
| `GET /databases/{databaseID}/jobs?limit=50` | `{"jobs":[...]}`, newest first; `limit` is 1 to 100 |
| `GET /databases/{databaseID}/jobs/{jobID}` | `{"job":{...}}` |

```json
{
  "job": {
    "id": "6f1c...",
    "execution_id": "0b7e...",
    "database_id": "2d4a...",
    "migration_id": "9c31...",
    "name": "add audit table",
    "actor_id": "51e8...",
    "action": "apply",
    "revision": 1,
    "status": "pending",
    "attempts": 0,
    "progress": {"committed_batches": 0, "committed_rows": 0},
    "available_at": "2026-11-14T09:00:00Z",
    "created_at": "2026-11-14T09:00:00Z"
  }
}
```

`last_error`, `started_at`, and `finished_at` appear once set. The queued SQL
and trace context are not returned. Errors use `{"error":"..."}` with `400`
for an invalid `limit`, `401`/`403` from access control, `404` for unknown
jobs, and `500` for storage failures.

## Tests

```sh
go test ./internal/job ./internal/worker
go test -tags=integration -count=1 ./tests -run '^(TestJob|TestMigrationWorker)'
```
