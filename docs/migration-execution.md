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
go test ./internal/job
go test -tags=integration -count=1 ./tests -run '^TestJob'
```
