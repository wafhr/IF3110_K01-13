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
| `committed_batches`, `committed_rows`, `total_rows` | Committed progress of batch migrations |
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

## Batch migrations

A migration revision with a `batch` setting runs per key range. The SQL must be
a single statement designed for batching: it receives the first and last key
of a batch as `$1` and `$2`, and both are inclusive.

```json
{
  "name": "raise balances",
  "sql_up": "UPDATE public.accounts SET balance_cents = balance_cents + 100 WHERE id BETWEEN $1 AND $2",
  "sql_down": "UPDATE public.accounts SET balance_cents = balance_cents - 100 WHERE id BETWEEN $1 AND $2",
  "batch": {"table": "public.accounts", "key_column": "id", "size": 10000}
}
```

- `table` is `table` or `schema.table`; `key_column` must be unique and never
  change, typically the primary key. Rows whose key is NULL are never
  processed. `size` is 1 to 100,000 rows.
- The batch settings belong to the immutable revision, so they are reviewed
  and approved together with the SQL.

### Execution

The worker holds the database advisory lock for the whole job, so no other
migration of the database runs between batch commits.

1. On the first attempt the scope is fixed: the current highest key and the
   row count. Rows inserted later above that key are never processed.
2. Each batch runs in its own transaction: `PrepareExecution` revalidates
   permission, approval, and the plan; the next range is read with
   `key > last_key AND key <= scope_end ORDER BY key LIMIT size`; the SQL runs
   for that range; the checkpoint row is inserted; then the transaction commits.
3. Only key boundaries are read, never whole rows, so memory use does not grow
   with the table.
4. When no range is left, a final transaction writes the execution history and
   marks the scope completed. History therefore contains the migration once,
   only after every batch committed.

### Checkpoints

The worker creates two tables next to `platform_migration_executions` on the
target the first time a batch migration runs there; later attempts only read
and write them:

| Table | Content |
|---|---|
| `platform_migration_batch_scopes` | Execution, migration, revision, batch settings, `scope_end`, `total_rows`, `status` (`running`/`completed`) |
| `platform_migration_batches` | One row per committed batch: number, `first_key`, `last_key`, `row_count`, commit time |

A batch row is written in the transaction of the batch it describes, so it is
the commit evidence of that batch. A new attempt continues after the last
batch row: committed batches never run twice, even after a worker crash, a
lost commit response, or a duplicate job delivery. The stored ranges are the
row scope that batch compensation uses.

### Progress, partial status, and resume

- After every batch commit the job's `progress` shows `committed_batches`,
  `committed_rows`, and `total_rows`.
- A transient failure is retried from the checkpoint. If a batch fails
  permanently, or retries run out, after at least one batch committed, the job
  becomes `partial`: committed batches stay, the failed batch is rolled back,
  and `last_error` describes the failure.
- A `partial` job holds every later job of the same database.
- `POST /databases/{databaseID}/jobs/{jobID}/resume` (View + Edit) queues a
  partial job again with a fresh attempt budget. The worker continues after
  the last committed batch and revalidates permission and approval for every
  batch. A job that is not `partial` returns `409`.
- Rolling back a batch migration is batch compensation and fails with
  `rollback of batch migrations runs as batch compensation` until
  compensation is available.

### Test dataset

`scripts/seed-batch-data.sql` creates `batch_benchmark_accounts` with 500,000
rows. `TestMigrationBatchBenchmarkIntegration` loads that file, times one
full-table `UPDATE` in a single transaction, then runs the same change as a
batch migration through the queue and worker.

```powershell
docker compose -f compose.test.yaml up -d --wait postgres-test
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'
$env:BATCH_BENCHMARK = '1'
go test -tags=integration -count=1 -timeout 15m -v ./tests -run '^TestMigrationBatchBenchmarkIntegration$'
```

Results on 14 November 2026, PostgreSQL 18 in Docker Desktop (8 CPUs, 2.8 GiB
memory) on Windows 11:

| Run | Rows | Duration |
|---|---|---|
| One `UPDATE ... SET balance_cents = balance_cents + 100, updated_at = now()` | 500,000 | 7.2 s |
| Same change as a batch migration, 50 batches of 10,000 | 500,000 | 12.8 s |

The batch run takes longer in total because every batch revalidates the job
and commits separately, but each transaction locks only 10,000 rows and keeps
its progress if a later batch fails.

## Job status API

Reading jobs requires at least View Only access to the database; resuming a
partial job requires View + Edit. A job is only reachable through the database
it was queued for.

| Method and path | Response |
|---|---|
| `GET /databases/{databaseID}/jobs?limit=50` | `{"jobs":[...]}`, newest first; `limit` is 1 to 100 |
| `GET /databases/{databaseID}/jobs/{jobID}` | `{"job":{...}}` |
| `POST /databases/{databaseID}/jobs/{jobID}/resume` | `202` with `{"job":{...}}`; requires View + Edit and the CSRF token |

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
    "progress": {"committed_batches": 0, "committed_rows": 0, "total_rows": 0},
    "available_at": "2026-11-14T09:00:00Z",
    "created_at": "2026-11-14T09:00:00Z"
  }
}
```

`last_error`, `started_at`, and `finished_at` appear once set. The queued SQL
and trace context are not returned. Errors use `{"error":"..."}` with `400`
for an invalid `limit`, `401`/`403` from access control, `404` for unknown
jobs, `409` when resuming a job that is not partial, and `500` for storage
failures.

## Tests

```sh
go test ./internal/job ./internal/worker
go test -tags=integration -count=1 ./tests -run '^(TestJob|TestMigrationWorker|TestMigrationBatch)'
```
